package launcher

import (
	"context"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
)

func processAttributes() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
}

// reapOrphans collects zombies reparented to this process while it is PID 1.
// It never waits on the supervised child itself: that exit status belongs to
// cmd.Wait, and a zombie has already released every descriptor, so reaping
// cannot weaken the inherited-lock proof.
func reapOrphans(ctx context.Context, child int) {
	if os.Getpid() != 1 {
		return
	}
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGCHLD)
	defer signal.Stop(signals)
	for {
		select {
		case <-ctx.Done():
			return
		case <-signals:
		}
		for _, pid := range zombieChildren(child) {
			var status syscall.WaitStatus
			_, _ = syscall.Wait4(pid, &status, syscall.WNOHANG, nil)
		}
	}
}

// zombieChildren lists defunct processes whose parent is this process, other
// than the supervised child.
func zombieChildren(child int) []int {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	self := os.Getpid()
	var zombies []int
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid == child || pid == self {
			continue
		}
		stat, err := os.ReadFile("/proc/" + entry.Name() + "/stat")
		if err != nil {
			continue
		}
		// "pid (comm) state ppid ..."; comm may contain spaces or parentheses.
		rest := string(stat)
		if i := strings.LastIndexByte(rest, ')'); i >= 0 {
			rest = rest[i+1:]
		}
		fields := strings.Fields(rest)
		if len(fields) < 2 || fields[0] != "Z" {
			continue
		}
		if ppid, err := strconv.Atoi(fields[1]); err == nil && ppid == self {
			zombies = append(zombies, pid)
		}
	}
	return zombies
}
