#!/usr/bin/env bash
# Bootstrap matching, checksum-verified releases. Helper-capable releases use
# a permanent kernel gate; known older releases use exact-owner mkdir exclusion.
set -u
script="${BASH_SOURCE[0]}"

# No public argument can enter a watchdog path. The only entry accepts one
# version, and every kill0 below is reached exclusively through the isolated
# asynchronous job at the end of this file, never by private-mode dispatch.
[ "$#" -eq 1 ] || exit 2
valid_version='^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?(\+[0-9A-Za-z.-]+)?$'
[[ "$1" =~ $valid_version ]] || exit 2
want="$1"
controller="$$"

# These functions are serialized by declare -f, then loaded only after the fresh
# child proves its kernel group. Static call-graph reachability cannot see that.
# shellcheck disable=SC2317
watchdog() {
	local budget remaining
	# An ordinary anonymous pipeline supplies stdin: no FIFO/filesystem open
	# or external tool is needed before this first builtin timeout is armed.
	IFS= read -r -t 1 budget || { kill -KILL 0; exit 1; }
	case "$budget" in *[!0-9]* | '') kill -KILL 0; exit 1 ;; esac
	[ "${#budget}" -le 3 ] || { kill -KILL 0; exit 1; }
	remaining=$((10#$budget - 1))
	[ "$remaining" -ge 1 ] || { kill -KILL 0; exit 1; }
	[ "$remaining" -le 599 ] || { kill -KILL 0; exit 1; }
	# Only the budget line is legal. EOF, another line, or the single remaining
	# deadline ends the ENTIRE cohort, including all phase watchdogs/tools.
	IFS= read -r -t "$remaining" budget || true
	kill -KILL 0
}

# Serialized helper, loaded only after the kernel group check above.
# shellcheck disable=SC2317
probe() {
	local limit="$1" result
	shift
	# An ordinary foreground pipeline is joined on Bash 3.2 as well. Its
	# producer emits nothing until the bounded command has fully completed;
	# the timed reader therefore detects a stall before any result line exists.
	# Job control stays OFF: both sides and all tools retain the global group.
	result="$(
		(
			output="$("$@" 2>/dev/null | head -c 8192)"
			status=$?
			printf '%s\n__tmag_exit_%d\n' "$output" "$status"
		) | (
			IFS= read -r -t "$limit" line || { kill -KILL 0; exit 1; }
			printf '%s\n' "$line"
			while IFS= read -r line; do printf '%s\n' "$line"; done
		)
	)"
	REPLY=; PROBE_STATUS=1
	case "$result" in
	*$'\n__tmag_exit_'*)
		PROBE_STATUS="${result##*$'\n__tmag_exit_'}"
		case "$PROBE_STATUS" in *[!0-9]* | '') PROBE_STATUS=1; return 1 ;; esac
		REPLY="${result%$'\n__tmag_exit_'*}"
		[ "$PROBE_STATUS" = 0 ]; return "$?" ;;
	*) return 1 ;;
	esac
}

# Serialized helper, loaded only after the kernel group check above.
# shellcheck disable=SC2317
worker() {
	set -o pipefail
	tmp=
	tmp_name=
	scratch_pinned=0
	finish() {
		local status="$1" message="${2:-}"
		# Only the exclusively created, pinned scratch is cleaned. A replaced entry
		# or ambiguous crash leftover is preserved; no prefix scan/recursive rm.
		if [ "$scratch_pinned" = 1 ] && [ . -ef /dev/fd/9 ]; then
			rm -f archive checksums candidate
			if cd .. && [ "$tmp_name" -ef /dev/fd/9 ]; then rmdir "$tmp_name" 2>/dev/null || true; fi
		fi
		if [ -n "$message" ]; then probe 1 tmux display-message -d 5000 "tmag: $message" || true; fi
		trap '' PIPE
		printf '__tmag_result_%d\n' "$status" >&6
		# Kills the watchdog and surviving tool descendants. Missing/partial
		# receipts mean failure, including a signal racing an already committed mv.
		kill -KILL 0
		exit 1
	}

	probe 1 tmux show-option -gqv @tmag-install-timeout || finish 1
	budget="${REPLY:-60}"
	case "$budget" in
	 off) finish 0 ;;
	 *[!0-9]* | '') finish 1 "invalid @tmag-install-timeout (use 1..600 seconds or off)" ;;
	esac
	[ "${#budget}" -le 3 ] || finish 1 "invalid @tmag-install-timeout"
	budget=$((10#$budget))
	[ "$budget" -ge 1 ] || finish 1 "invalid @tmag-install-timeout"
	[ "$budget" -le 600 ] || finish 1 "invalid @tmag-install-timeout"
	[ "$budget" -gt 1 ] || finish 1
	# The global watchdog charges one second for policy lookup. Its remaining
	# deadline is never reset by a phase, receipt, notification or helper startup.
	printf '%s\n' "$budget"
	dir="$(cd -P "${script%/*}/.." && pwd -P)" || finish 1
	bin="$dir/bin/tmag"

	if probe 2 "$bin" version; then
		case "$REPLY" in "tmag $want ("*")") finish 0 ;; esac
	fi
	os="$(uname -s)" || finish 1
	case "$os" in Linux) os=linux ;; Darwin) os=darwin ;; *) finish 1 "unsupported platform" ;; esac
	case "$(uname -m)" in
	 x86_64 | amd64) arch=amd64 ;;
	 aarch64 | arm64) arch=arm64 ;;
	 *) finish 1 "unsupported architecture" ;;
	esac
	probe 1 tmux show-option -gqv @tmag-release-url || finish 1
	base="${REPLY:-https://github.com/ChrAlpha/tmux-agent-experience/releases/download}"
	asset="tmag_${want}_${os}_${arch}.tar.gz"
	umask 077
	# Execute verified candidates on the same filesystem as the plugin binary;
	# a noexec or unavailable TMPDIR is not a new installation prerequisite.
	cd -P "$dir" || finish 1
	[ -O . ] || finish 1
	exec 4<. || finish 1
	[ ! -L bin ] || finish 1
	if [ ! -d bin ]; then mkdir bin || finish 1; fi
	cd -P bin || finish 1
	[ -O . ] || finish 1
	exec 8<. || finish 1
	[ "$dir" -ef /dev/fd/4 ] || finish 1
	[ "$dir/bin" -ef /dev/fd/8 ] || finish 1
	tmp_name="$(mktemp -d ./.install-bootstrap.XXXXXXXX)" || finish 1
	tmp_name="${tmp_name##*/}"
	tmp="$dir/bin/$tmp_name"
	cd -P "$tmp_name" || finish 1
	exec 9<. || finish 1
	scratch_pinned=1
	namespace_current() {
		[ "$dir" -ef /dev/fd/4 ] && [ ! -L "$dir/bin" ] &&
			[ "$dir/bin" -ef /dev/fd/8 ] && [ ! -L "$tmp" ] && [ "$tmp" -ef /dev/fd/9 ]
	}
	manifest_current() {
		local path="$dir/plugins/codex/tmag/.codex-plugin/plugin.json" parent
		for parent in plugins plugins/codex plugins/codex/tmag plugins/codex/tmag/.codex-plugin; do
			[ -d "$dir/$parent" ] && [ ! -L "$dir/$parent" ] || return 1
		done
		[ -f "$path" ] && [ ! -L "$path" ] && [ -O "$path" ] || return 1
		# A bounded standard-awk JSON recognizer avoids a jq/runtime dependency
		# for old binaries. Version must be one literal top-level string.
		head -c 65537 "$path" | LC_ALL=C awk -v want="$want" '
			function ws() { while (p <= length(text) && substr(text,p,1) ~ /[ \t\r\n]/) p++ }
			function quoted( start,c,i,h) {
				ws(); if (substr(text,p++,1) != "\"") return 0; start=p
				while (p <= length(text)) {
					c=substr(text,p++,1)
					if (c == "\"") { token=substr(text,start,p-start-1); return 1 }
					if (c ~ /[[:cntrl:]]/) return 0
					if (c == "\\") {
						c=substr(text,p++,1)
						if (c == "u") { for (i=0;i<4;i++) { h=substr(text,p++,1); if (h !~ /^[0-9a-fA-F]$/) return 0 } }
						else if (c != "\"" && c != "\\" && c != "/" && c !~ /^[bfnrt]$/) return 0
					}
				}
				return 0
			}
			function value(depth, c,key,closing,rest) {
				if (depth > 64) return 0
				ws(); c=substr(text,p,1)
				if (c == "\"") return quoted()
				if (c == "{" || c == "[") {
					closing=(c == "{" ? "}" : "]"); p++; ws()
					if (substr(text,p,1) == closing) { p++; return 1 }
					while (1) {
						if (c == "{") {
							if (!quoted()) return 0; key=token
							if (depth == 0 && index(key,"\\")) return 0
							ws(); if (substr(text,p++,1) != ":") return 0
						}
						if (c == "{" && depth == 0 && key == "version") {
							if (!quoted()) return 0; versions++; if (token != want) bad=1
						} else if (!value(depth+1)) return 0
						ws(); if (substr(text,p,1) == closing) { p++; return 1 }
						if (substr(text,p++,1) != ",") return 0
					}
				}
				rest=substr(text,p)
				if (match(rest,/^(true|false|null)/)) { p+=RLENGTH; return 1 }
				if (match(rest,/^-?(0|[1-9][0-9]*)(\.[0-9]+)?([eE][+-]?[0-9]+)?/)) { p+=RLENGTH; return 1 }
				return 0
			}
			{ text=text $0 "\n" }
			END {
				p=1; ws()
				if (length(text)>65536 || substr(text,p,1)!="{" || !value(0)) exit 1
				ws(); if (p<=length(text) || versions!=1 || bad) exit 1
			}
		'
	}
	controller_present() {
		ps -A -o pid= -o ppid= -o state= | LC_ALL=C awk -v child="$$" -v owner="$controller" '
			NF { if (NF!=3 || $1 !~ /^[0-9]+$/ || $2 !~ /^[0-9]+$/ || ++rows>65536) { bad=1; exit } }
			NF==3 && $3 !~ /^[ZX]/ { parents[$1]=$2 }
			END {
				if (bad) exit 1
				for (i=0;i<256 && child>1;i++) {
					if (!(child in parents)) exit 1
					if (child==owner) exit 0
					child=parents[child]
				}
				exit 1
			}
		'
	}
	legacy_publish() (
		# This whole function runs in a subshell. Keep trap state in that shell,
		# rather than depending on function-local unwind ordering at EXIT.
		legacy_lock_owned=0
		legacy_marker_owned=0
		legacy_digest=
		legacy_trap_depth="$BASH_SUBSHELL"
		legacy_cleanup() {
			# Only the creator shell cleans up, even if an EXIT trap is visible
			# in a pipeline/command-substitution child on another Bash release.
			[ "$BASH_SUBSHELL" = "$legacy_trap_depth" ] || return 0
			[ "$legacy_lock_owned" = 1 ] && [ "$legacy_marker_owned" = 1 ] || return 0
			if [ . -ef /dev/fd/8 ]; then
				[ ! -L .install.lock ] && [ .install.lock -ef /dev/fd/7 ] || return 0
				cd .install.lock || return 0
			fi
			[ . -ef /dev/fd/7 ] && [ ! -L owner ] && [ owner -ef /dev/fd/3 ] || return 0
			rm -f owner || return 0
			cd .. || return 0
			[ . -ef /dev/fd/8 ] && [ ! -L .install.lock ] && [ .install.lock -ef /dev/fd/7 ] || return 0
			rmdir .install.lock 2>/dev/null || true
		}
		namespace_current || return 1
		cd .. || return 1
		[ . -ef /dev/fd/8 ] || return 1
		# Never adopt/expire an existing directory or downgrade a regular gate.
		mkdir .install.lock 2>/dev/null || return 1
		[ ! -L .install.lock ] && [ -d .install.lock ] && [ -O .install.lock ] || return 1
		exec 7<.install.lock || return 1
		[ .install.lock -ef /dev/fd/7 ] && [ -d /dev/fd/7 ] || return 1
		cd .install.lock || return 1
		[ . -ef /dev/fd/7 ] || return 1
		legacy_lock_owned=1
		trap legacy_cleanup EXIT
		[ ! -e owner ] && [ ! -L owner ] || return 1
		(set -C; printf 'tmag/install/legacy-v1\n%s\n' "$tmp_name" >owner) || return 1
		[ -f owner ] && [ ! -L owner ] && [ -O owner ] || return 1
		exec 3<owner || return 1
		[ owner -ef /dev/fd/3 ] || return 1
		legacy_marker_owned=1
		namespace_current && manifest_current || return 1
		[ -f "$tmp/candidate" ] && [ ! -L "$tmp/candidate" ] || return 1
		if command -v sha256sum >/dev/null 2>&1; then
			legacy_digest="$(sha256sum "$tmp/candidate")" || return 1
		else
			legacy_digest="$(shasum -a 256 "$tmp/candidate")" || return 1
		fi
		[ "${legacy_digest%% *}" = "${image_sum%% *}" ] || return 1
		chmod 755 "$tmp/candidate" || return 1
		probe 1 controller_present || return 1
		namespace_current && manifest_current || return 1
		cd .. || return 1
		[ . -ef /dev/fd/8 ] && [ ! -L .install.lock ] && [ .install.lock -ef /dev/fd/7 ] || return 1
		[ "$tmp_name" -ef /dev/fd/9 ] && [ -f "$tmp_name/candidate" ] && [ ! -L "$tmp_name/candidate" ] || return 1
		[ ! -L tmag ] || return 1
		if [ -e tmag ]; then [ -f tmag ] && [ -O tmag ] || return 1; fi
		# Bootstrap and destination are inside this pinned bin filesystem. The
		# prior executable is untouched until the single rename commit point.
		mv -f "$tmp_name/candidate" tmag
	)
	fetch() {
		local url="$1" target="$2" blocks="$3"
		(
			# Conservative limits account for system-shell -f unit differences:
			# at most 64 MiB for archive, 1 MiB for checksums on Linux/macOS.
			ulimit -f "$blocks" || exit 1
			if command -v curl >/dev/null 2>&1; then
				curl -q -fsSL --connect-timeout 10 --max-time "$budget" --max-redirs 5 -o "$target" -- "$url"
			else
				wget --no-config --tries=1 --timeout=10 --max-redirect=5 -qO "$target" -- "$url"
			fi
		)
	}
	failure="release $want was not confirmed; inspect bin/tmag and recover unknown locks only after stopping old installers"
	fetch "$base/v$want/$asset" archive 65536 2>/dev/null || finish 1 "$failure"
	fetch "$base/v$want/checksums.txt" checksums 1024 2>/dev/null || finish 1 "$failure"
	expected="$(awk -v name="$asset" '
		NF == 2 && $2 == name { n++; if (length($1) != 64 || $1 !~ /^[[:xdigit:]]+$/) bad=1; hash=tolower($1) }
		END { if (n != 1 || bad) exit 1; print hash }
	' checksums)" || finish 1 "$failure"
	if command -v sha256sum >/dev/null 2>&1; then
		actual="$(sha256sum archive)" || finish 1 "$failure"
	else
		actual="$(shasum -a 256 archive)" || finish 1 "$failure"
	fi
	[ "${actual%% *}" = "$expected" ] || finish 1 "$failure"
	set -o pipefail
	# No archive pathname is extracted. Only one regular root member can become
	# executable; -O prevents links or traversal from writing archive pathnames.
	tar -tzf archive | awk '
		$0 == "tmag" { n++; next }
		$0 == "LICENSE" || $0 == "README.md" { next }
		{ bad=1 }
		END { if (bad || n != 1) exit 1 }
	' || finish 1 "$failure"
	tar -tvzf archive tmag | awk 'substr($0,1,1) != "-" { bad=1 } END { if (NR != 1 || bad) exit 1 }' || finish 1 "$failure"
	(ulimit -f 131072 && tar -xOzf archive tmag >candidate) || finish 1 "$failure"
	[ -s candidate ] || finish 1 "$failure"
	chmod 700 candidate || finish 1 "$failure"
	if command -v sha256sum >/dev/null 2>&1; then
		image_sum="$(sha256sum candidate)" || finish 1 "$failure"
	else
		image_sum="$(shasum -a 256 candidate)" || finish 1 "$failure"
	fi
	probe 2 ./candidate version || finish 1 "$failure"
	case "$REPLY" in "tmag $want ("*")") ;; *) finish 1 "$failure" ;; esac
	# Capability selection is read-only. A publishing helper error NEVER changes
	# publisher: only the exact known old-version usage contract selects legacy.
	namespace_current || finish 1 "$failure"
	capability_check() { ./candidate __install-capabilities 2>&1; }
	if probe 2 capability_check; then
		[ "$REPLY" = tmag/install/v1 ] || finish 1 "$failure"
		namespace_current || finish 1 "$failure"
		./candidate __install-release "$dir" "$want" "$controller" "${image_sum%% *}" >/dev/null 2>&1 || finish 1 "$failure"
	else
		[ "$want" = 0.3.0 ] || finish 1 "$failure"
		[ "$PROBE_STATUS" = 2 ] || finish 1 "$failure"
		if command -v sha256sum >/dev/null 2>&1; then
			cap_sum="$(printf '%s' "$REPLY" | sha256sum)" || finish 1 "$failure"
		else
			cap_sum="$(printf '%s' "$REPLY" | shasum -a 256)" || finish 1 "$failure"
		fi
		# v0.3.0 internal/cli/cli.go: exact unknown-command stderr + usage,
		# with only trailing newlines removed by command substitution.
		[ "${cap_sum%% *}" = 7d84ac68ead04ecf0a075d9d9553c9d764f5853d3e488cfcbc51d1372a9a61b4 ] || finish 1 "$failure"
		legacy_publish || finish 1 "$failure"
	fi
	finish 0 "installed $want; connect agents with the configured setup key (default prefix + M-a)"
}

# A fresh Bash job establishes its group, then verifies the kernel group named
# by its OWN live PID exists before loading any watchdog code. A newly forked
# PID cannot name a preexisting unrelated group. Failed job-control setup thus
# exits without any timer or kill0, rather than trusting argv/env or a mode bit.
# -p suppresses startup files/function imports; it grants no new OS privileges.
trap 'exit 1' HUP INT TERM
result=
exec 5< <(
	exec 2>/dev/null || exit 1
	set -m || exit 1
	[[ $- == *m* ]] || exit 1
	exec 6>&1 || exit 1
	definitions="$(builtin declare -f watchdog probe worker)" || exit 1
	# Expand child parameters only in the fresh, kernel-verified shell.
	# shellcheck disable=SC2016
	"$BASH" --noprofile --norc -p -c '
		set -u
		[ "$#" -eq 4 ] || exit 1
		builtin kill -0 -- "-$$" 2>/dev/null || exit 1
		set +m
		# This is only the source of the three functions declared above, never
		# interpolated user data. Loading it occurs after kernel group proof.
		builtin eval "$4" || exit 1
		want="$1"; controller="$2"; script="$3"
		worker </dev/null 2>/dev/null | watchdog 2>/dev/null
	' tmag-installer "$want" "$controller" "$script" "$definitions" &
	wait "$!" 2>/dev/null
) || exit 1
IFS= read -r result <&5 || exit 1
# EOF follows the wrapper's ordinary wait on its isolated job. Do not rely on
# wait($!) for process substitutions, which Bash 3.2 does not reliably expose
# in its jobs table. Any extra receipt line is an invalid outcome.
while IFS= read -r _extra <&5; do result=invalid-receipt; done
exec 5<&-
[ "$result" = __tmag_result_0 ] && exit 0
exit 1
