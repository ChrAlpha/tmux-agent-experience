#!/usr/bin/env bash
# tmux-agent-experience: agent awareness inside tmux.
#
# Installs the tmag binary next to this file, then sets up key bindings,
# alerts, status-line placeholders and the hooks that record the turns you see.
# Every default can be changed or turned off with the options below; nothing
# outside the plugin directory is written.
#
#   @tmag-key-picker  a           prefix key for a tree of panes with agents
#   @tmag-key-jump    A           prefix key to jump to the agent waiting longest
#   @tmag-key-setup   M-a         prefix key to connect the coding agents installed, then check
#   @tmag-key-serve   M-r         prefix key to open the local HTTP API in a window
#   @tmag-serve       (unset)     arguments for tmag serve: --addr, --host, --origin
#   @tmag-alert       blocked,finished   alert on these changes: ring a pane's bell, run @tmag-notify
#   @tmag-notify      (unset)     command that tells you of an alert away from the terminal
#   @tmag-answer      on          who may decide prompts with `tmag answer`: on (people), agents, off
#   @tmag-seen        on          remember the finished turns you see at the terminal; off leaves only `tmag seen`
#   @tmag-peers       on          tmag setup gives agents the tmag-peers skill, to work with each other
#   #{tmag_status}               placeholder for status-left / status-right
#
# Set a key option to "off" to leave that key unbound, @tmag-alert to "off" to
# disable bells, @tmag-answer to "off" to answer prompts only in the agent,
# @tmag-seen to "off" to mark turns seen only yourself with `tmag seen`, and
# @tmag-peers to "off" to keep tmag out of the agents' context.

dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
tmag="$dir/scripts/tmag"

option() {
	local value
	value="$(tmux show-option -gqv "$1")"
	echo "${value:-$2}"
}

version="$(sed -n 's/.*"version": *"\([^"]*\)".*/\1/p' "$dir/plugins/codex/tmag/.codex-plugin/plugin.json")"
tmux set-option -g @tmag-plugin-version "$version"

# The worker checks the installed version in the background, so an unresponsive
# executable cannot block tmux's plugin entry point.
tmux run-shell -b "'$dir/scripts/install.sh' '$version'"

# Agent hooks read this to decide when to ring a pane's bell.
tmux set-option -g @tmag-alert "$(option @tmag-alert blocked,finished)"

# `tmag answer` reads this to decide who may answer an agent's prompts, and
# Claude Code's permission hook whether to wait for one beside its dialog.
tmux set-option -g @tmag-answer "$(option @tmag-answer on)"

# Agents' hooks and `tmag status` read this to decide whether to record a
# finished turn the person is looking at as seen.
seen="$(option @tmag-seen on)"
tmux set-option -g @tmag-seen "$seen"

# `tmag setup` reads this to install the tmag-peers plugin in the agents, or
# to remove it.
tmux set-option -g @tmag-peers "$(option @tmag-peers on)"

# A pane's live state, for pane-border-format and friends. It is empty once the
# agent that wrote it has exited: at once when another program takes the pane,
# and at the next tmag hook, status refresh or picker when an agent of the same
# name replaced it without the first reporting its exit.
if [ -z "$(tmux show-option -gqv @tmag-pane-status)" ]; then
	tmux set-option -g @tmag-pane-status '#{?#{&&:#{@tmag_state},#{==:#{pane_current_command},#{@tmag_cmd}}},#{@tmag_agent} #{@tmag_state}#{?#{@tmag_kind}, #{@tmag_kind},},}'
fi

# Status-line placeholder, interpolated the way TPM plugins conventionally do.
placeholder='#{tmag_status}'
replacement="#('$tmag' status --tmux --server '#{socket_path}')"
for name in status-left status-right; do
	value="$(tmux show-option -gqv "$name")"
	case "$value" in
	*"$placeholder"*) tmux set-option -g "$name" "${value//"$placeholder"/$replacement}" ;;
	esac
done

live='#{&&:#{@tmag_state},#{==:#{pane_current_command},#{@tmag_cmd}}}'

# Seen turns follow the view: when a session's current window or a window's
# active pane changes, or a client attaches or switches session, the hook marks
# the finished turns that view shows. Each hook is one array member at an index
# of tmag's own, in the scope tmux keeps it: global for session and client
# events, global window for pane changes. The person's hooks keep theirs;
# loading the plugin again replaces tmag's member in place, and @tmag-seen off
# removes it.
seen_marker='# tmag seen hook'
seen_scope() { # hook-name
	case "$1" in
	window-pane-changed) echo -gw ;;
	*) echo -g ;;
	esac
}
seen_index() { # hook-name scope
	local name="$1" scope="$2" line used index
	line="$(tmux show-hooks "$scope" 2>/dev/null | grep -F -- "$seen_marker" | grep -E "^$name\[[0-9]+\]" | head -n 1)"
	if [ -n "$line" ]; then
		index="${line#*[}"
		echo "${index%%]*}"
		return
	fi
	used="$(tmux show-hooks "$scope" 2>/dev/null | sed -n "s/^$name\[\([0-9]*\)\].*/\1/p")"
	index=97
	while printf '%s\n' "$used" | grep -qx "$index"; do
		index=$((index + 1))
	done
	echo "$index"
}
seen_remove() { # hook-name scope
	local name="$1" scope="$2" line index flags
	line="$(tmux show-hooks "$scope" 2>/dev/null | grep -F -- "$seen_marker" | grep -E "^$name\[[0-9]+\]" | head -n 1)"
	[ -n "$line" ] || return 0
	index="${line#*[}"
	case "$scope" in
	-gw) flags=-guw ;;
	*) flags=-gu ;;
	esac
	tmux set-hook "$flags" "${name}[${index%%]*}]"
}

seen_filter='#{&&:#{==:#{@tmag_state},idle},#{==:#{pane_current_command},#{@tmag_cmd}}}'
for seen_hook in session-window-changed window-pane-changed client-session-changed client-attached; do
	scope="$(seen_scope "$seen_hook")"
	if [ "$seen" = off ]; then
		seen_remove "$seen_hook" "$scope"
		continue
	fi
	seen_command="if-shell -F '$seen_filter' 'run-shell -b \"\\\"$tmag\\\" seen --shown --server \\\"#{socket_path}\\\" $seen_marker\"'"
	tmux set-hook "$scope" "${seen_hook}[$(seen_index "$seen_hook" "$scope")]" "$seen_command"
done

# The picker filters on records alone, so it first clears those of agents that
# left their pane without a word; run-shell finishes before choose-tree runs.
key="$(option @tmag-key-picker a)"
if [ "$key" != off ]; then
	tmux bind-key -N "Choose an agent" "$key" run-shell "'$tmag' prune --server '#{socket_path}'" '\;' \
		choose-tree -Z -f "$live" \
		-F "#{?pane_format,#{@tmag_agent} #{@tmag_state}#{?#{@tmag_kind}, #{@tmag_kind},}  #{pane_current_path},#{?window_format,#{window_name},#{session_windows} windows}}"
fi

key="$(option @tmag-key-jump A)"
if [ "$key" != off ]; then
	tmux bind-key -N "Jump to the agent waiting longest" "$key" \
		run-shell "'$tmag' jump --client '#{client_name}' --server '#{socket_path}' --from '#{pane_id}'"
fi

key="$(option @tmag-key-setup M-a)"
if [ "$key" != off ]; then
	tmux bind-key -N "Connect the coding agents to tmag" "$key" \
		display-popup -E -w 80% -h 80% "'$tmag' setup; echo; '$tmag' doctor; printf '\\nPress Enter to close.'; read -r _"
fi

# The optional local HTTP API runs in a window of its own, for as long as that
# window lives; the key opens it, or tells where it runs.
key="$(option @tmag-key-serve M-r)"
if [ "$key" != off ]; then
	tmux bind-key -N "Open the local tmag HTTP API" "$key" \
		run-shell "'$tmag' serve --open --server '#{socket_path}'"
fi
