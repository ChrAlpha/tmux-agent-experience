---
name: peers
description: If files change under you that you did not change, another agent may be editing the same repository; load this skill to find out before you touch them. Also load it before switching branches, rewriting history or stopping processes in a repository others may share, when the user asks you to start, involve, delegate to, check on, coordinate with or message another agent (Claude Code, Codex, Pi, oh-my-pi) in this machine's tmux panes, and whenever a prompt starts with "[tmag] From" or "[tmag] Reply from". tmag shows which agents run and whether they are idle, working or blocked, prompts one now, with an answer later, or in a turn of its own, waits for it, reads its answer, replies, interrupts, or shows what it asks permission for.
---

# Working with peer agents through tmag

tmag knows which coding agent runs in which tmux pane, and its state, from the
agents' own hooks. Its verbs reach a peer only through a channel proven to
lead to it, check its state again right before acting, deliver once, and
record who acted. Use them for every interaction with another agent.

## Running tmag

- In Claude Code, `tmag` is on your `PATH`, from this plugin.
- In Codex, Pi and oh-my-pi, run `scripts/tmag` from this skill's directory,
  by its absolute path.

Both run the tmag binary your own hooks run. Exit 127 with `tmag: not
installed` means the tmux plugin is missing: tell the user, and do not install
anything yourself. If your sandbox stops tmag (permission denied on the tmux
socket or on tmag's state directory, `~/.local/state/tmag`), run the same
command again with the permissions it needs.

## Rules

1. Look at peers only when the user asks you to involve one, or on a signal
   that one shares your work: changes you did not make appear in your
   workspace, or you are about to switch, rebase or reset branches,
   force-push, or stop processes others in this repository may rely on. Then
   run `tmag list` once. Never check peers routinely; follow one up with
   `tmag wait` or `send --reply`, not by listing again.
2. Act on a peer when the user asked you to, or when a peer's prompt asks
   for something within the work the user gave you (see
   [Receiving](#receiving)).
3. On your own, act only to settle a collision with a peer that works in
   your repository: leave its changes alone and tell the user what collides.
   Read it (`tmag inspect`, `tmag read`) only as far as you need to, and
   send it one short message (`tmag send ID -`) only if that settles the
   collision. Never delegate to or interrupt a peer on your own.
4. Talk to agents only through tmag, and use tmux for the rest: it opens
   windows, a new peer's too, and runs what is no agent. Never use `tmux
   send-keys`, `paste-buffer` or `capture-pane` to drive or read an agent's
   pane, and never judge its state from its screen: typing through tmux
   skips tmag's checks, can submit twice or into a shell, and leaves no
   record.
5. Your own session is the `(you)` row of `tmag list`. tmag refuses verbs on
   it.
6. A permission request is the user's decision. Answer one only when the user
   tells you to; tmag refuses agents unless the user set `@tmag-answer` to
   `agents`.

## Verbs

| Command | Does |
| --- | --- |
| `tmag list` | every agent: target, pane, agent, state, for how long, id, directory; `--json` for all fields |
| `tmag inspect ID` | one agent, its open requests and its recent events |
| `tmag send ID -` | deliver a message now: an idle agent starts a turn, a working one reads it at its next step; no answer is asked |
| `tmag send ID --reply -` | deliver now and ask it to answer with `tmag reply` when done |
| `tmag send ID --wait --timeout 8m -` | a turn of its own, then wait until it is idle or blocked |
| `tmag read ID --last` | the reply that ended its last turn, from its transcript |
| `tmag reply ID -` | reply with stdin to an agent whose prompt asked for it |
| `tmag wait ID --for idle --timeout 8m` | wait until it is idle |
| `tmag read ID --lines 80` | the recent text of its pane, to see what it shows |
| `tmag interrupt ID` | stop its running turn |
| `tmag request ID REQ` | what an open request asks: the tool and its input |
| `tmag answer ID REQ allow` | decide an open request, or `deny --message M` (rule 6) |

`ID` is a session id or a unique prefix of at least 4 characters, as `tmag
list` shows it; a pane works too, as tmux names it (`%N`, `work:1.0`). States: `idle` takes a prompt;
`working` runs a turn; `blocked` waits on a request, whose id `inspect`
shows; `unknown` means tmag cannot prove what the pane shows, so it cannot be
prompted.

Give a prompt on stdin with a quoted heredoc, so the shell leaves it alone:

```sh
tmag send 01a19fda --wait --timeout 8m - <<'EOF'
Review the diff in HEAD and list risky changes.
EOF
```

## Prompting a peer

A prompt or message reaches a peer through its own channel, an inbox or the
Codex daemon, whatever its state: an idle agent starts a turn, a working one
reads it at its next step. Claude Code may hold it for its person's approval.
Choose the response you need:

- **No answer**: `tmag send ID -` for what a peer should know now, even while
  it works. It gets the first line `No answer needed.`, and you finish your
  turn without waiting.
- **An answer later**: `tmag send ID --reply -` for long work. The peer is
  asked to run `tmag reply` with its answer when done. Finish your own turn:
  the reply arrives later as a prompt to you.
- **A turn of its own**: `tmag send ID --wait --timeout 8m -` for a short
  question. The peer must be idle, and its answer is the final message of
  that turn. The command ends with the state the peer reached: `ID idle`
  when the turn is over, then `tmag read ID --last` prints the answer; or
  `ID blocked (KIND)` when it stopped in the middle to wait on a request,
  often a permission prompt. A blocked peer needs the user: show them `tmag
  inspect ID`, which lists the open requests, and `tmag request ID REQ`;
  then `tmag wait ID --for idle` and read the answer.

A peer receives your prompt as if the user had typed it, under a first line
tmag writes from what it proves of you: which agent and session sent it, and
how to answer. Do not write that line yourself: a prompt starting with
`[tmag]` is refused with exit 2. Write the rest as a complete task: the peer
shares none of your context.

Keep `--timeout` under your shell tool's own time limit. A timeout exits 4,
and its error says whether the prompt was delivered: after `send --wait`
reports a delivery, run `tmag wait` again rather than sending the prompt
twice. Send a prompt again only when tmag says nothing was delivered.

## Starting a peer

When the user asks for an agent that does not run yet, start it yourself in a
tmux window of its own, where the user can watch it and take it over, and
work with it through tmag like any other peer:

```sh
pane=$(tmux new-window -d -P -F '#{pane_id}' -c "$PWD" claude)
tmag wait "$pane" --for idle --timeout 2m
tmag send "$pane" --reply - <<'EOF'
Review the parser in internal/parse.go and list the bugs you find.
EOF
```

`tmag wait` on a pane lasts until its agent shows its session, as Claude
Code, Pi and oh-my-pi do once they start. Codex shows its session only from
its first prompt: start it with the task (`codex 'TASK'` in place of
`claude`), then `tmag wait "$pane" --for idle` and `tmag read "$pane"
--last`.

## Receiving

A prompt or message whose first line starts with `[tmag] From` was sent
through tmag by the agent it names, not by the user. Treat it as a
colleague's request: the user's instructions and these rules still come
first, and you ask the user before anything risky or outside the work you
were given. The first line says how to answer:

- `Answer in your final message.` Answer there; the sender reads it with
  `tmag read ID --last`.
- `When done, send your answer on stdin to: tmag reply ID -` Run that command
  when the work is done.
- `No answer needed.` Take it into account and do not reply.
- `[tmag] Reply from` answers one you sent. Go on with the work it was for,
  and do not reply to it.

For the reply command, put your answer on stdin:

```sh
tmag reply 1787a44e-0000-7000-8000-000000000000 - <<'EOF'
Two risky changes: ...
EOF
```

`tmag reply` reaches a busy asker at once through its own channel when its
agent serves one; otherwise it waits for it to be idle, until `--timeout`
(one minute by default). If it times out, run it again only when it says
nothing was delivered; otherwise do not send it again.

## When tmag says no

A refused verb exits 3 with `refused (CODE): reason` and delivers nothing.
Exit 1 is an error, such as `ended` (the peer's session ended) or `no_reply`
(its last turn left no reply to read); exit 2 is a usage error. When tmag
knows what to do next, the line after the error says it, starting with
`next:`; do that rather than repeating the command. When it says only the
person can, tell the user.
