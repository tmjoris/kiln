package agent

import (
	"os"
	"os/signal"
	"sync"

	"golang.org/x/sys/unix"
)

// reaper owns every wait4() in the process. As PID 1 the agent inherits all
// orphaned processes, so it must reap pids it never started; exec.Cmd.Wait
// would race with that, so commands register here instead.
type reaper struct {
	mu      sync.Mutex
	waiters map[int]chan unix.WaitStatus
}

func newReaper() *reaper {
	r := &reaper{waiters: map[int]chan unix.WaitStatus{}}
	sig := make(chan os.Signal, 64)
	signal.Notify(sig, unix.SIGCHLD)
	go func() {
		for range sig {
			r.reapAll()
		}
	}()
	return r
}

func (r *reaper) reapAll() {
	for {
		var ws unix.WaitStatus
		pid, err := unix.Wait4(-1, &ws, unix.WNOHANG, nil)
		if pid <= 0 || err != nil {
			return
		}
		r.mu.Lock()
		ch, ok := r.waiters[pid]
		delete(r.waiters, pid)
		r.mu.Unlock()
		if ok {
			ch <- ws
		}
		// Unknown pids are orphans re-parented to us; reaping is all they need.
	}
}

// start runs startFn with the reaper locked, so a child that exits instantly
// cannot be reaped before its waiter is registered.
func (r *reaper) start(startFn func() (int, error)) (<-chan unix.WaitStatus, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	pid, err := startFn()
	if err != nil {
		return nil, err
	}
	ch := make(chan unix.WaitStatus, 1)
	r.waiters[pid] = ch
	return ch, nil
}
