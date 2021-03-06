package exec

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"strings"

	bpf "github.com/iovisor/gobpf/bcc"
	"github.com/jessfraz/bpfd/proc"
	"github.com/jessfraz/bpfd/program"
	"github.com/jessfraz/bpfd/types"
)

const (
	name = "exec"
	// This is heavily based on: https://github.com/iovisor/bcc/blob/master/tools/execsnoop.py
	source string = `
#include <uapi/linux/ptrace.h>
#include <linux/sched.h>
#include <linux/fs.h>
#define ARGSIZE  128
#define MAXARG 20
enum event_type {
    EVENT_ARG,
    EVENT_RET,
};
struct data_t {
    u32 pid;  // PID as in the userspace term (i.e. task->tgid in kernel)
    u32 tgid; // Parent PID as in the userspace term (i.e task->real_parent->tgid in kernel)
    char comm[TASK_COMM_LEN];
    enum event_type type;
    char argv[ARGSIZE];
    int retval;
};
BPF_PERF_OUTPUT(events);
static int __submit_arg(struct pt_regs *ctx, void *ptr, struct data_t *data)
{
    bpf_probe_read(data->argv, sizeof(data->argv), ptr);
    events.perf_submit(ctx, data, sizeof(struct data_t));
    return 1;
}
static int submit_arg(struct pt_regs *ctx, void *ptr, struct data_t *data)
{
    const char *argp = NULL;
    bpf_probe_read(&argp, sizeof(argp), ptr);
    if (argp) {
        return __submit_arg(ctx, (void *)(argp), data);
    }
    return 0;
}
int syscall__execve(struct pt_regs *ctx,
    const char __user *filename,
    const char __user *const __user *__argv,
    const char __user *const __user *__envp)
{
    // create data here and pass to submit_arg to save stack space (#555)
    struct data_t data = {};
    struct task_struct *task;
    data.pid = bpf_get_current_pid_tgid() >> 32;
    task = (struct task_struct *)bpf_get_current_task();
    // Some kernels, like Ubuntu 4.13.0-generic, return 0
    // as the real_parent->tgid.
    // We use the get_tgid function as a fallback in those cases. (#1883)
    data.tgid = task->real_parent->tgid;
    bpf_get_current_comm(&data.comm, sizeof(data.comm));
    data.type = EVENT_ARG;
    __submit_arg(ctx, (void *)filename, &data);
    // skip first arg, as we submitted filename
    #pragma unroll
    for (int i = 1; i < MAXARG; i++) {
        if (submit_arg(ctx, (void *)&__argv[i], &data) == 0)
             goto out;
    }
    // handle truncated argument list
    char ellipsis[] = "...";
    __submit_arg(ctx, (void *)ellipsis, &data);
out:
    return 0;
}
int do_ret_sys_execve(struct pt_regs *ctx)
{
    struct data_t data = {};
    struct task_struct *task;
    data.pid = bpf_get_current_pid_tgid() >> 32;
    task = (struct task_struct *)bpf_get_current_task();
    // Some kernels, like Ubuntu 4.13.0-generic, return 0
    // as the real_parent->tgid.
    // We use the get_tgid function as a fallback in those cases. (#1883)
    data.tgid = task->real_parent->tgid;
    bpf_get_current_comm(&data.comm, sizeof(data.comm));
    data.type = EVENT_RET;
    data.retval = PT_REGS_RC(ctx);
    events.perf_submit(ctx, &data, sizeof(data));
    return 0;
}
`
)

type execEvent struct {
	PID         uint32
	TGID        uint32
	Comm        [16]byte
	Type        int32
	Argv        [128]byte
	ReturnValue int32
}

func init() {
	program.Register(name, Init)
}

type bpfprogram struct {
	module  *bpf.Module
	perfMap *bpf.PerfMap
	channel chan []byte
	argv    map[uint32][]string
}

// Init returns a new bashreadline program.
func Init() (program.Program, error) {
	return &bpfprogram{
		channel: make(chan []byte),
		argv:    map[uint32][]string{},
	}, nil
}

func (p *bpfprogram) String() string {
	return name
}

func (p *bpfprogram) Load() error {
	p.module = bpf.NewModule(source, []string{})

	execKprobe, err := p.module.LoadKprobe("syscall__execve")
	if err != nil {
		return fmt.Errorf("load sys_execve kprobe failed: %v", err)
	}

	execve := bpf.GetSyscallFnName("execve")
	err = p.module.AttachKprobe(execve, execKprobe)
	if err != nil {
		return fmt.Errorf("attach sys_execve kprobe: %v", err)
	}

	execKretprobe, err := p.module.LoadKprobe("do_ret_sys_execve")
	if err != nil {
		return fmt.Errorf("load sys_execve kretprobe failed: %v", err)
	}

	err = p.module.AttachKretprobe(execve, execKretprobe)
	if err != nil {
		return fmt.Errorf("attach sys_execve kretprobe: %v", err)
	}

	table := bpf.NewTable(p.module.TableId("events"), p.module)

	p.perfMap, err = bpf.InitPerfMap(table, p.channel)
	if err != nil {
		return fmt.Errorf("init perf map failed: %v", err)
	}

	return nil
}

func (p *bpfprogram) WatchEvent(rules []types.Rule) (*program.Event, error) {
	var event execEvent
	data := <-p.channel
	if err := binary.Read(bytes.NewBuffer(data), binary.LittleEndian, &event); err != nil {
		return nil, fmt.Errorf("failed to decode received data: %v", err)
	}

	index := bytes.IndexByte(event.Argv[:], 0)
	if index <= -1 {
		index = 128
	}
	argv := strings.TrimSpace(string(event.Argv[:index]))

	if event.Type == 0 {
		if len(argv) > 0 {
			// This is an event arg.
			// Append it to the other args.
			p.argv[event.PID] = append(p.argv[event.PID], argv)
		}
		return nil, nil
	}

	if event.Type != 1 {
		// Return early if not a return event.
		return nil, nil
	}

	// Convert C string (null-terminated) to Go string
	command := strings.TrimSpace(string(event.Comm[:bytes.IndexByte(event.Comm[:], 0)]))

	runtime := proc.GetContainerRuntime(int(event.TGID), int(event.PID))

	e := &program.Event{PID: event.PID, TGID: event.TGID, Data: map[string]string{
		"argv":      strings.Join(p.argv[event.PID], " "),
		"command":   command,
		"returnval": fmt.Sprintf("%d", event.ReturnValue),
		"type":      fmt.Sprintf("%d", event.Type),
	}}

	// Delete from the array of argv.
	delete(p.argv, event.PID)

	// Verify the event matches for the rules.
	if program.Match(rules, e.Data, runtime) {
		e.Data["runtime"] = string(runtime)
		e.Data["container_id"] = proc.GetContainerID(int(event.PID))
		return e, nil
	}

	// We didn't find what we were searching for so return nil.
	return nil, nil
}

func (p *bpfprogram) Start() {
	p.perfMap.Start()
}

func (p *bpfprogram) Unload() {
	if p.perfMap != nil {
		p.perfMap.Stop()
	}
	if p.module != nil {
		p.module.Close()
	}
}
