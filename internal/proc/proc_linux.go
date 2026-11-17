//go:build linux

package proc

import (
	"context"
	"os"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
)

// readProcFS reads every process through /proc with raw system calls.
// It reads stat for every pid, cmdline only for names that can be an agent or
// launcher, and walks pids with a small bounded worker pool.
func readProcFS(ctx context.Context) ([]Process, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if _, err := os.Stat("/proc/self/stat"); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	var pids []int
	for _, e := range entries {
		if pid, err := strconv.Atoi(e.Name()); err == nil {
			pids = append(pids, pid)
		}
	}
	ps := make([]Process, len(pids))
	ok := make([]bool, len(pids))
	var next atomic.Int64
	var wg sync.WaitGroup
	var mu sync.Mutex
	var firstErr error
	workers := min(8, runtime.GOMAXPROCS(0))
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				i := int(next.Add(1) - 1)
				if i >= len(pids) {
					return
				}
				if err := ctx.Err(); err != nil {
					mu.Lock()
					if firstErr == nil {
						firstErr = err
					}
					mu.Unlock()
					return
				}
				p, found := readProcEntry(ctx, pids[i])
				if !found {
					if ctx.Err() != nil {
						mu.Lock()
						if firstErr == nil {
							firstErr = ctx.Err()
						}
						mu.Unlock()
						return
					}
					if _, err := os.Stat("/proc/" + strconv.Itoa(pids[i])); os.IsNotExist(err) {
						continue
					}
					p = Process{PID: pids[i], Uncertain: true}
				}
				ps[i], ok[i] = p, true
			}
		}()
	}
	wg.Wait()
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if firstErr != nil {
		return nil, firstErr
	}
	out := make([]Process, 0, len(pids))
	for i, p := range ps {
		if ok[i] {
			out = append(out, p)
		}
	}
	return out, nil
}

// readProcLineage reads pid and its ancestors up to the nearest agent
// client or service and that host's parent. The table is incomplete by
// construction: it proves nothing about processes outside the lineage.
func readProcLineage(ctx context.Context, pid int) (*Table, error) {
	if pid <= 0 {
		return nil, os.ErrInvalid
	}
	var ps []Process
	seen := map[int]bool{}
	hostAt := -1
	for pid > 0 && !seen[pid] {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		p, ok := readProcEntry(ctx, pid)
		if !ok {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if len(ps) == 0 {
				return nil, os.ErrInvalid
			}
			break
		}
		ps = append(ps, p)
		seen[pid] = true
		if hostAt >= 0 {
			break // the host's parent, enough to tell a Claude worker apart
		}
		if ServiceKind(p) != "" || AgentKind(p) != "" {
			hostAt = p.PID
		}
		pid = p.PPID
	}
	return newIncompleteTable(ps), nil
}

// readProcEntry reads one /proc entry: stat always, cmdline only when
// the executable name can be an agent or launcher.
func readProcEntry(ctx context.Context, pid int) (Process, bool) {
	dir := "/proc/" + strconv.Itoa(pid)
	stat, err := readFileContext(ctx, dir+"/stat", 64<<10)
	if err != nil {
		return Process{}, false
	}
	p, ok := parseProcStat(pid, stat)
	if !ok {
		return Process{}, false
	}
	if candidateCommand(p.Comm) {
		if cmdline, err := readFileContext(ctx, dir+"/cmdline", 4<<20); err == nil {
			setCommandLine(&p, cmdline)
		}
	}
	return p, true
}

// readFileContext reads a /proc file with raw system calls, stopping at limit.
func readFileContext(ctx context.Context, path string, limit int) ([]byte, error) {
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	defer syscall.Close(fd)
	buf := make([]byte, 0, min(limit, 4096))
	tmp := make([]byte, 4096)
	for len(buf) < limit {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		n, err := syscall.Read(fd, tmp)
		if n > 0 {
			buf = append(buf, tmp[:min(n, limit-len(buf))]...)
		}
		if err == syscall.EINTR {
			continue
		}
		if err != nil {
			return nil, err
		}
		if n == 0 {
			break
		}
	}
	return buf, nil
}
