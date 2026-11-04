---
name: install-tmag
description: Install, update or verify tmux-agent-experience, the tmux plugin that tracks coding agents, and connect the Claude Code or Codex session you are running in.
---

# tmux-agent-experience install skill

**Canonical URL**: `https://raw.githubusercontent.com/ChrAlpha/tmux-agent-experience/main/SKILL.md`
**Audience**: coding agents running in Claude Code or Codex, inside tmux.
**Purpose**: install or update the tmux plugin, connect the agent you are
running in, and verify. Every step is safe to repeat.

A user typically hands you this document with a prompt such as:

> Read https://raw.githubusercontent.com/ChrAlpha/tmux-agent-experience/main/SKILL.md and follow it to install or update tmag for the AI tool I am using.

## Agent rules

1. **Never guess.** If the operating system, the host you run in, or a path is
   unclear, ask the user one short question and wait.
2. **Change hosts only through their own commands.** Agent plugins change
   through `tmag setup`, which runs `claude plugin …` and `codex plugin …`.
   Never hand-edit agent settings, hook files or `config.toml`.
3. **Ask before touching the user's environment.** Editing tmux.conf,
   installing TPM, and restarting the Codex daemon (it interrupts running
   Codex work) each need an explicit yes. Show the exact change first.
4. **No sudo.** Everything installs into the user's home.
5. **Verify before you claim.** Success is `tmag doctor` without `✗` lines,
   checked again after the restart in Step 5.
6. **One round of recovery, then hand back.** Read the error verbatim, apply
   the matching fix in [Triage](#triage) once; if it fails again, stop and
   report the exact error and the step you were on.
7. **Reply in the user's language** for every message you send during this
   loop. Commands, output and paths stay as they are.

## The loop

```
0. Check prerequisites and find TPM.     (Step 0)
1. Add the plugin to tmux.conf.          (Step 1)
2. Install or update it through TPM.     (Step 2)
3. Connect the agent you run in.         (Step 3)
4. Restart the agent, then verify.       (Step 4)
```

## Step 0: Prerequisites

```sh
uname -sm                                  # Linux or Darwin; x86_64/amd64 or arm64/aarch64
tmux -V                                    # 3.2 or newer
command -v curl || command -v wget         # one of them
echo "$TMUX"                               # non-empty: you run inside tmux
tmux show-environment -g TMUX_PLUGIN_MANAGER_PATH
```

- Windows is supported only inside WSL. If tmux is missing or older than 3.2,
  stop: installing tmux is the user's call. If `$TMUX` is empty, ask the user
  to start you in a tmux pane.
- The tmux config is `~/.tmux.conf`, or `~/.config/tmux/tmux.conf` when that
  file exists. Call it `CONF` below.
- TPM is installed when `TMUX_PLUGIN_MANAGER_PATH` is set and its directory
  contains `tpm/`. Call the directory `PLUGINS` (it ends in `/`). If TPM is
  missing, ask before installing it:

  ```sh
  git clone https://github.com/tmux-plugins/tpm ~/.tmux/plugins/tpm
  ```

  and before appending `run '~/.tmux/plugins/tpm/tpm'` as the **last** line of
  `CONF`. Then `tmux source-file CONF` and read `PLUGINS` again.

## Step 1: Add the plugin

If `CONF` already contains `ChrAlpha/tmux-agent-experience`, skip this step.
Otherwise ask, then add this line **above** TPM's `run` line:

```tmux
set -g @plugin 'ChrAlpha/tmux-agent-experience'
```

## Step 2: Install or update through TPM

```sh
"${PLUGINS}tpm/bin/install_plugins"                              # install
"${PLUGINS}tpm/bin/update_plugins" tmux-agent-experience         # update
tmux source-file CONF                                            # load it
```

On load the plugin downloads the released binary for its version, checks its
SHA-256 and puts it at `${PLUGINS}tmux-agent-experience/bin/tmag`. It runs in
the background; wait for it (up to a minute):

```sh
TMAG="${PLUGINS}tmux-agent-experience/bin/tmag"
for i in $(seq 60); do "$TMAG" version 2>/dev/null && break; sleep 1; done
```

If it never appears, read `tmux show-messages` and see [Triage](#triage).

## Step 3: Connect the agent

| Host | Signal |
| --- | --- |
| Claude Code | You are Claude Code; `CLAUDECODE` is set in your shell. |
| Codex | You are Codex; `CODEX_THREAD_ID` is set in your shell. |

```sh
"$TMAG" setup --yes --agent claude     # or --agent codex
```

`setup` installs or updates tmag's plugins through the agent's own plugin
manager and prints what is left to do: `tmag`, whose hooks report the agent's
state, and `tmag-peers`, a skill for working with the other agents through
tmag. With `@tmag-peers` set to `off` in tmux, it leaves out `tmag-peers`, or
removes it. If the other agent is also installed
(`command -v claude`, `command -v codex`), ask whether to connect it as well.
Other hosts are not supported: tmag still lists their panes, but cannot know
their state.

## Step 4: Restart and verify

1. Run `"$TMAG" doctor`. Act on any `✗` via [Triage](#triage). Before the
   restart, `!` on `state` and `agents` is expected.
2. **Codex only:** if the `codex` line says the daemon cannot find tmag, the
   shared daemon started outside tmux before the plugin existed. It must be
   restarted with `codex app-server daemon restart` from a tmux pane, which
   interrupts running Codex work. If you are Codex, that includes your own
   session: hand the command to the user instead of running it. Otherwise ask
   first.
3. Tell the user to end this session and start a fresh one in a tmux pane;
   hooks load at session start. In Codex, they approve tmag's hooks with
   `/hooks`.
4. **Codex only:** Codex runs threads in a shared daemon, where only the
   terminal title can show which pane displays which thread. Ask the user to
   run `/title` in Codex once and turn on `thread-id`; Codex saves the choice
   itself. Do not edit Codex's configuration for them. Codex started with
   `--no-daemon` needs no setting.
5. In the fresh session, `"$TMAG" doctor` should show the plugin loaded and the
   agent's `tmag` and `tmag-peers` plugins enabled (or `tmag-peers off`), and
   `"$TMAG" list` should show that pane with the session's id:
   - Claude Code: state `idle` right away; `list --json` gives the pane
     evidence `process`.
   - Codex: after the first prompt, evidence `title-id` (or `process` with
     `--no-daemon`). Before it the pane shows `unknown` without an id, which
     is expected.

Report the final `tmag doctor` lines, and tell the user about the keys:
`prefix + A` jumps to the agent waiting longest, `prefix + a` lists agents,
`prefix + M-a` re-runs setup and doctor, and `#{tmag_status}` can go in their
status line. Tell them too that agents can now work with each other through
tmag when asked to, and that `set -g @tmag-peers off` followed by
`prefix + M-a` turns that off.

## Triage

| Symptom | What to do |
| --- | --- |
| `bin/tmag` never appears | `tmux show-messages` names the cause, usually no network access to GitHub. Fix access and `tmux source-file CONF`; a mirror can be set with `@tmag-release-url`. Never build tmag from source as a workaround. |
| `doctor`: tmux plugin not loaded | `CONF` lacks the `@plugin` line, or it sits below TPM's `run` line. Fix the order and `tmux source-file CONF`. |
| `doctor`: tmux plugin version differs from the binary | `tmux source-file CONF` to let the plugin fetch the matching binary. |
| `doctor`: `tmux` needs 3.2 or newer | Stop; the user upgrades tmux. |
| `setup` fails on a plugin command | Read the error verbatim. An unknown flag means an outdated host: ask the user to update Claude Code or Codex. |
| `doctor`: Codex hooks not approved, or Codex agents stay `unknown` after a plugin update although an approval is found | Codex honours an approval only for the hooks it was given for. After a restart, approve them in Codex with `/hooks`. |
| `doctor`: hook errors recorded | Report the latest line; it names the failure. |
| `tmag list` shows a Codex pane without an id long after a turn, and the session on its own row | The terminal title lacks the thread id. The user runs `/title` in Codex and turns on `thread-id`; the pane attaches at once. |

## Uninstall

`"$TMAG" uninstall` prints every removal step. Run them only after the user
confirms; run `"$TMAG" uninstall --purge` first, while the binary exists.

## What this skill does not do

- It does not install or upgrade tmux, Go or the agents.
- It does not restart the agent; the user does.
- It does not edit agent configuration files; plugins change through each
  agent's own plugin commands.
- It edits tmux.conf only with the user's explicit consent.

---

**Last updated**: 2026-11-04
