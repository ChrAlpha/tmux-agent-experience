package query

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/ChrAlpha/tmux-agent-experience/internal/adapter"
	"github.com/ChrAlpha/tmux-agent-experience/internal/eventlog"
	"github.com/ChrAlpha/tmux-agent-experience/internal/helper"
	"github.com/ChrAlpha/tmux-agent-experience/internal/proc"
	"github.com/ChrAlpha/tmux-agent-experience/internal/session"
	"github.com/ChrAlpha/tmux-agent-experience/internal/tmux"
)

// Start opens a tmux window for an agent beside the pane tmag runs in, where
// the person can watch it and take it over, and leaves it there: the agent
// is tmux's, not tmag's. A start names an agent, or a template the person
// set as @tmag-start-NAME; nobody else gives tmag a command to run.

// TemplateOption prefixes the tmux options that hold the person's commands
// for tmag start, one per name.
const TemplateOption = "@tmag-start-"

// WorktreesOption is the tmux option naming the directory, under the
// repository's main worktree unless absolute, that tmag start --worktree
// adds worktrees in.
const WorktreesOption = "@tmag-worktrees"

// templateName is what a template may be called, and windowName what a
// started agent's window may be, so that the name is a target tmag takes.
var (
	templateName = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)
	windowName   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,31}$`)
)

// ErrNoWindow reports a start from no tmux pane: tmag opens the agent's
// window beside the pane it runs in.
var ErrNoWindow = errors.New("tmag start opens a window beside the tmux pane it runs in, and runs in none")

// StartOptions say what to start, where, and the first prompt.
type StartOptions struct {
	Agent    string // claude, codex, pi or omp, or
	Template string // the NAME of @tmag-start-NAME
	Dir      string // the directory; "" for that of the pane tmag runs in
	Worktree string // a branch to start in a worktree of its own
	Name     string // the window's name; "" for the one tmux gives it
	Text     string // the first prompt; "" for none
	Ask      Ask
}

// Started is what a start opened: the agent's pane, the session it shows,
// and the delivery of its first prompt, if one went through tmag's verbs
// (Codex takes its first prompt with its command).
type Started struct {
	Pane     eventlog.PaneRef `json:"pane"`
	Session  Session          `json:"session"`
	Dir      string           `json:"dir"`
	Seq      int64            `json:"seq"`
	Delivery *Delivery        `json:"-"`
}

// codexFirst is the first prompt tmag gives a Codex it starts, with its
// command: Codex shows its session only from its first prompt, so its task
// follows, through send, once it does.
const codexFirst = "tmag started you in this tmux window. Reply ready, and wait for the next prompt."

// Start opens a window for the agent o names in o.Dir, waits until its pane
// shows its session, logs the Start event, and gives it o.Text as send
// would. A pane that shows no session before ctx ends is a NotShownError:
// the window stays, for the person to see what holds it.
func Start(ctx context.Context, log *eventlog.Log, o StartOptions) (Started, error) {
	text := strings.TrimRight(tmux.SanitizePrompt(o.Text), "\n")
	switch first, _, _ := strings.Cut(strings.TrimLeft(text, " \t\n"), "\n"); {
	case o.Text != "" && strings.TrimSpace(text) == "":
		return Started{}, fmt.Errorf("%w: the prompt is empty", errUsage)
	case strings.HasPrefix(strings.ToLower(first), Marker):
		return Started{}, ErrMarker
	case o.Name != "" && (!windowName.MatchString(o.Name) || !isName(o.Name)):
		return Started{}, fmt.Errorf("%w: a window is named by up to 32 letters, digits, - and _, starting with a letter or digit, and not by digits alone", errUsage)
	}
	v, w, caller, err := prepareWorld(ctx, log)
	if err != nil {
		return Started{}, err
	}
	here, ok := w.Enclosing(os.Getpid())
	if !ok {
		return Started{}, ErrNoWindow
	}
	srv := tmux.Server{Socket: here.Server}
	agent, command, err := startCommand(ctx, srv, o)
	if err != nil {
		return Started{}, err
	}
	if o.Name != "" {
		// A name names one window of the session, or none at all.
		if n, err := srv.WindowsNamed(ctx, "="+here.Session, o.Name); err != nil {
			return Started{}, fmt.Errorf("reading the windows of %s: %w", here.Session, err)
		} else if n > 0 {
			return Started{}, fmt.Errorf("%w: a window of tmux session %s is named %s already, and a name names one window", errUsage, here.Session, o.Name)
		}
	}
	dir := o.Dir
	if dir == "" {
		dir = here.Path
	}
	if info, err := os.Stat(dir); err != nil || !info.IsDir() || !filepath.IsAbs(dir) {
		return Started{}, fmt.Errorf("%w: %q is no absolute path of a directory", errUsage, dir)
	}
	// A prompt is checked as far as it can be before the window opens: one
	// refused afterwards would leave an agent that has nothing to do.
	if text != "" {
		if _, err := header(caller, o.Ask); err != nil {
			return Started{}, err
		}
		if _, err := deepened(ctx, v, caller); err != nil && (o.Ask == AskReply || o.Ask == AskFinal) {
			return Started{}, err
		}
	}
	if agent == proc.Codex {
		command += " -- " + tmux.Quote(codexFirst)
	}
	if o.Worktree != "" {
		if dir, err = worktree(ctx, srv, dir, o.Worktree); err != nil {
			return Started{}, err
		}
	}
	// The agent replaces the shell tmux runs its command with, as it does a
	// person's shell, so it holds the pane's foreground.
	id, err := srv.NewWindow(ctx, here, dir, o.Name, "exec "+command)
	if err != nil {
		return Started{}, fmt.Errorf("opening a window for %s: %w", agent, err)
	}
	pane := eventlog.PaneRef{Server: here.Server, ID: id}
	st := Started{Pane: pane, Dir: dir}
	store := session.Store{Dir: log.Dir}
	if st.Session, err = WaitShown(ctx, store, pane); err != nil {
		if ctx.Err() != nil {
			err = &NotShownError{Pane: pane, Err: ctx.Err()}
		}
		return st, err
	}
	if st.Seq, err = logStart(ctx, log, st.Session, pane, dir, o.Template, caller); err != nil {
		err = fmt.Errorf("%w: %w", ErrUnlogged, err)
		if text != "" {
			err = errors.Join(ErrNotGiven, err)
		}
		return st, &StartedError{Session: st.Session, Err: err}
	}
	if text == "" {
		return st, nil
	}
	if agent == proc.Codex {
		// Its task waits for the turn of its first prompt to end; the pane
		// may show the session before that turn starts.
		if _, err := Wait(ctx, store, st.Session, WaitOptions{States: []string{adapter.Idle}, Turned: true}); err != nil {
			return st, &StartedError{Session: st.Session, Err: errors.Join(ErrNotGiven, err)}
		}
	}
	d, err := Send(ctx, log, st.Session.ID, text, o.Ask)
	st.Delivery = &d
	if err != nil {
		err = &StartedError{Session: st.Session, Err: err}
	}
	return st, err
}

// ErrNotGiven reports a started agent its prompt was not given to, and
// ErrUnlogged one whose Start event could not be logged.
var (
	ErrNotGiven = errors.New("its prompt was not given")
	ErrUnlogged = errors.New("its start was not logged")
)

// StartedError reports a start whose agent started, and whose prompt then
// met Err: starting it again would start another.
type StartedError struct {
	Session Session
	Err     error
}

func (e *StartedError) Error() string {
	return fmt.Sprintf("%s started as %s, then: %v", e.Session.Agent, e.Session.ID, e.Err)
}

func (e *StartedError) Unwrap() error { return e.Err }

// NotShownError reports a started agent whose pane showed no session in
// time: a trust dialog, a login or a missing thread-id title item may hold
// it.
type NotShownError struct {
	Pane eventlog.PaneRef
	Err  error
}

func (e *NotShownError) Error() string {
	return fmt.Sprintf("the agent started in %s, which shows no session tmag can prove yet: %v", e.Pane.ID, e.Err)
}

func (e *NotShownError) Unwrap() error { return e.Err }

// errUsage marks a start whose options are wrong.
var errUsage = errors.New("usage")

// IsUsage reports a start refused for its options.
func IsUsage(err error) bool { return errors.Is(err, errUsage) }

// startCommand is the agent o names and the command its window runs: the
// agent's own name, or the template's command, whose first word must run
// one of the agents.
func startCommand(ctx context.Context, srv tmux.Server, o StartOptions) (agent, command string, err error) {
	if o.Template == "" {
		if !proc.IsAgent(o.Agent) {
			return "", "", fmt.Errorf("%w: start one of %s, or a template", errUsage, strings.Join(proc.Agents, ", "))
		}
		return o.Agent, o.Agent, nil
	}
	if !templateName.MatchString(o.Template) {
		return "", "", fmt.Errorf("%w: a template is named by lowercase letters, digits, - and _", errUsage)
	}
	command, err = srv.Option(ctx, TemplateOption+o.Template)
	if err != nil {
		return "", "", err
	}
	command = strings.TrimSpace(command)
	if command == "" {
		return "", "", fmt.Errorf("%w: no %s%s is set on this tmux server", errUsage, TemplateOption, o.Template)
	}
	fields := strings.Fields(command)
	agent = filepath.Base(fields[0])
	if !proc.IsAgent(agent) {
		return "", "", fmt.Errorf("%w: %s%s runs %s, which is none of %s", errUsage, TemplateOption, o.Template, fields[0], strings.Join(proc.Agents, ", "))
	}
	return agent, command, nil
}

// worktree is the directory of branch's worktree of the repository dir is
// in, under the main worktree's root for them: one git lists there on that
// branch, or one tmag adds, from dir's HEAD if the branch is new. tmag never
// removes one, nor takes as a worktree a directory git does not list.
func worktree(ctx context.Context, srv tmux.Server, dir, branch string) (string, error) {
	if err := gitRun(ctx, dir, "check-ref-format", "--branch", branch); err != nil {
		return "", fmt.Errorf("%w: %q is no branch name", errUsage, branch)
	}
	out, err := gitOutput(ctx, dir, "worktree", "list", "--porcelain")
	if err != nil {
		return "", fmt.Errorf("%w: %s is in no git repository", errUsage, dir)
	}
	trees := worktrees(out)
	if len(trees) == 0 || trees[0].bare {
		return "", fmt.Errorf("%w: the repository of %s has no main worktree", errUsage, dir)
	}
	root, _ := srv.Option(ctx, WorktreesOption)
	if root = strings.TrimSpace(root); root == "" {
		root = ".worktrees"
	}
	if !filepath.IsAbs(root) {
		root = filepath.Join(trees[0].path, root)
	}
	path := filepath.Join(root, filepath.FromSlash(branch))
	for _, t := range trees {
		if !sameDir(t.path, path) {
			continue
		}
		if t.branch != "refs/heads/"+branch {
			return "", fmt.Errorf("%w: %s is a worktree, not on %s", errUsage, path, branch)
		}
		return path, nil
	}
	if _, err := os.Lstat(path); err == nil {
		return "", fmt.Errorf("%w: %s exists and is no worktree of %s", errUsage, path, trees[0].path)
	}
	if gitRun(ctx, dir, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch) == nil {
		return path, gitRun(ctx, dir, "worktree", "add", path, branch)
	}
	return path, gitRun(ctx, dir, "worktree", "add", "-b", branch, path, "HEAD")
}

// tree is a worktree as git worktree list --porcelain lists it.
type tree struct {
	path, branch string
	bare         bool
}

// worktrees parses git worktree list --porcelain: the main worktree first.
func worktrees(out string) []tree {
	var trees []tree
	for _, block := range strings.Split(strings.TrimSpace(out), "\n\n") {
		var t tree
		for _, line := range strings.Split(block, "\n") {
			key, value, _ := strings.Cut(line, " ")
			switch key {
			case "worktree":
				t.path = value
			case "branch":
				t.branch = value
			case "bare":
				t.bare = true
			}
		}
		if t.path != "" {
			trees = append(trees, t)
		}
	}
	return trees
}

// sameDir reports whether a and b name the same directory, symbolic links
// resolved where they exist.
func sameDir(a, b string) bool {
	resolve := func(p string) string {
		if r, err := filepath.EvalSymlinks(p); err == nil {
			return r
		}
		return filepath.Clean(p)
	}
	return resolve(a) == resolve(b)
}

// gitRun runs git in dir.
func gitRun(ctx context.Context, dir string, args ...string) error {
	_, err := gitOutput(ctx, dir, args...)
	return err
}

// gitOutput runs git in dir and returns its output.
func gitOutput(ctx context.Context, dir string, args ...string) (string, error) {
	out, err := helper.Command(ctx, "git", append([]string{"-C", dir}, args...)...).Output()
	if err != nil {
		var exit interface{ ExitCode() int }
		if errors.As(err, &exit) {
			return "", fmt.Errorf("git %s: exit %d", args[0], exit.ExitCode())
		}
		return "", fmt.Errorf("git %s: %w", args[0], err)
	}
	return string(out), nil
}

// logStart keeps in the session's record the agent session that started it,
// if an agent's proven session did, and logs the Start event: who started
// it, in which pane and directory, and by which template.
func logStart(ctx context.Context, log *eventlog.Log, s Session, pane eventlog.PaneRef, dir, template string, c Caller) (int64, error) {
	store := session.Store{Dir: log.Dir}
	ev := eventlog.Event{Event: eventlog.Start, Agent: s.Agent, Session: s.ID, Cwd: dir, Pane: &pane, Template: template}
	c.stamp(&ev)
	if c.By != nil && c.By.Session != "" {
		by := asker(c)
		ev.StartedBy = &by
	}
	err := store.Lock(ctx, s.Agent, s.ID, func() error {
		rec, ok, err := store.Get(ctx, s.Agent, s.ID)
		if err != nil {
			return err
		}
		if ok && ev.StartedBy != nil {
			rec.StartedBy = ev.StartedBy
			if err := store.Put(ctx, rec); err != nil {
				return err
			}
		}
		return log.Append(ctx, &ev)
	})
	return ev.Seq, err
}
