package e2e

import (
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// keepEvidence copies what a failed test leaves behind into a directory of
// its own under $TMAG_E2E_EVIDENCE, which CI keeps: the state directory, the
// test's directory, each pane's options and screen, and the process table.
// Without the variable it keeps nothing.
func (h *harness) keepEvidence() {
	root := os.Getenv("TMAG_E2E_EVIDENCE")
	if root == "" {
		return
	}
	dir := filepath.Join(root, strings.ReplaceAll(h.t.Name(), "/", "_"))
	copyRegular(h.state, filepath.Join(dir, "state"))
	copyRegular(h.dir, filepath.Join(dir, "test"))
	panes := filepath.Join(dir, "panes")
	if err := os.MkdirAll(panes, 0o755); err != nil {
		return
	}
	run := func(name string, args ...string) {
		out, err := exec.Command(args[0], args[1:]...).CombinedOutput()
		if err != nil {
			out = append(out, "\n"+err.Error()+"\n"...)
		}
		_ = os.WriteFile(name, out, 0o644)
	}
	run(filepath.Join(panes, "list"), "tmux", "-L", h.name, "list-panes", "-a", "-F",
		"#{pane_id} #{session_name}:#{window_index}.#{pane_index} pid=#{pane_pid} dead=#{pane_dead} #{pane_current_command}")
	list, _ := exec.Command("tmux", "-L", h.name, "list-panes", "-a", "-F", "#{pane_id}").Output()
	for _, id := range strings.Fields(string(list)) {
		name := filepath.Join(panes, strings.TrimPrefix(id, "%"))
		run(name+".options", "tmux", "-L", h.name, "show-options", "-p", "-t", id)
		run(name+".screen", "tmux", "-L", h.name, "capture-pane", "-p", "-t", id)
	}
	run(filepath.Join(dir, "processes"), "ps", "-A", "-ww", "-o", "pid=,ppid=,pgid=,stat=,command=")
}

// copyRegular copies the regular files under from to to, skipping sockets
// and anything else a copy cannot hold.
func copyRegular(from, to string) {
	_ = filepath.WalkDir(from, func(path string, d fs.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(from, path)
		if err != nil {
			return nil
		}
		src, err := os.Open(path)
		if err != nil {
			return nil
		}
		defer src.Close()
		dst := filepath.Join(to, rel)
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return nil
		}
		out, err := os.Create(dst)
		if err != nil {
			return nil
		}
		defer out.Close()
		_, _ = io.Copy(out, src)
		return nil
	})
}
