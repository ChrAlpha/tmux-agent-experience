#!/usr/bin/env bash
# tmux-agent-experience: agent awareness inside tmux.
#
# Installs the tmag binary next to this file, then sets up key bindings,
# alerts and status-line placeholders. Every default can be changed or turned
# off with the options below; nothing outside the plugin directory is written.
#
#   @tmag-key-picker  a           prefix key for a tree of panes with agents
#   @tmag-key-jump    A           prefix key to jump to the agent waiting longest
#   @tmag-key-setup   M-a         prefix key to connect Claude Code and Codex, then check
#   @tmag-alert       blocked,finished   ring a pane's bell on these changes
#   @tmag-answer      on          who may decide prompts with `tmag answer`: on (people), agents, off
#   @tmag-peers       on          tmag setup gives agents the tmag-peers skill, to work with each other
#   #{tmag_status}               placeholder for status-left / status-right
#
# Set a key option to "off" to leave that key unbound, @tmag-alert to "off" to
# disable bells, @tmag-answer to "off" to answer prompts only in the agent,
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

# The binary lives in bin/; install or replace it when the plugin changed.
if [ "$("$dir/bin/tmag" version 2>/dev/null | awk '{print $2}')" != "$version" ]; then
	tmux run-shell -b "'$dir/scripts/install.sh' '$version'"
fi

# Agent hooks read this to decide when to ring a pane's bell.
tmux set-option -g @tmag-alert "$(option @tmag-alert blocked,finished)"

# `tmag answer` reads this to decide who may answer an agent's prompts, and
# Claude Code's permission hook whether to wait for one beside its dialog.
tmux set-option -g @tmag-answer "$(option @tmag-answer on)"

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
	tmux bind-key -N "Connect Claude Code and Codex to tmag" "$key" \
		display-popup -E -w 80% -h 80% "'$tmag' setup; echo; '$tmag' doctor; printf '\\nPress Enter to close.'; read -r _"
fi
