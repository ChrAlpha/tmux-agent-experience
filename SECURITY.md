# Security policy

tmag can type into other programs' terminals and records which agent runs
where, so some bugs are security bugs. Please report these privately:

- `tmag send` delivering to the wrong agent, to an agent that is not idle, or
  delivering keystrokes beyond the prompt text;
- a session bound to the wrong pane;
- prompt text, agent output or tool input reaching tmux options, the event log
  or `hook.log`;
- a hook that blocks, slows or alters an agent's turn, or writes to its stdout.

## Reporting

Use GitHub's private vulnerability reporting: the **Report a vulnerability**
button on the repository's Security tab. Include the tmag, tmux and agent
versions and `tmag doctor --json` output. Do not open a public issue.

You can expect an acknowledgement within a week. Fixes are released for the
latest version only.
