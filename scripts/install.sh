#!/usr/bin/env bash
# Puts the released tmag binary matching this plugin checkout into bin/.
#
# Usage: install.sh VERSION
#
# Downloads the release archive and installs it only if its SHA-256 matches the
# release's checksums.txt. Several tmux servers may load the plugin at once, so
# one installer runs at a time.
set -u

dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
want="$1"
bin="$dir/bin/tmag"
lock="$dir/bin/.install.lock"
# Messages go to attached clients only; output from a background run-shell could
# otherwise open in a pane. Without clients, tmag doctor reports the outcome.
say() { tmux display-message -d 5000 "tmag: $*" >/dev/null 2>&1 || true; }

installed() { [ "$("$bin" version 2>/dev/null | awk '{print $2}')" = "$want" ]; }
installed && exit 0

mkdir -p "$dir/bin"
# A lock left by a killed installer expires after ten minutes.
find "$lock" -maxdepth 0 -mmin +10 -exec rmdir {} \; 2>/dev/null
mkdir "$lock" 2>/dev/null || exit 0
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"; rmdir "$lock"' EXIT

fetch() {
	if command -v curl >/dev/null; then
		curl -fsSL "$1" -o "$2"
	else
		wget -qO "$2" "$1"
	fi
}

download() {
	local os arch asset base sum
	os="$(uname -s | tr '[:upper:]' '[:lower:]')"
	case "$(uname -m)" in
	x86_64 | amd64) arch=amd64 ;;
	aarch64 | arm64) arch=arm64 ;;
	*) return 1 ;;
	esac
	asset="tmag_${want}_${os}_${arch}.tar.gz"
	base="$(tmux show-option -gqv @tmag-release-url 2>/dev/null)"
	base="${base:-https://github.com/ChrAlpha/tmux-agent-experience/releases/download}"
	if command -v sha256sum >/dev/null; then sum="sha256sum"; else sum="shasum -a 256"; fi
	fetch "$base/v$want/$asset" "$tmp/$asset" 2>/dev/null &&
		fetch "$base/v$want/checksums.txt" "$tmp/checksums.txt" 2>/dev/null &&
		(cd "$tmp" && grep "  $asset\$" checksums.txt | $sum -c - >/dev/null 2>&1) &&
		tar -xzf "$tmp/$asset" -C "$tmp" tmag
}

if ! download; then
	say "could not download a verified release $want; see https://github.com/ChrAlpha/tmux-agent-experience/releases"
	exit 1
fi
install -m 755 "$tmp/tmag" "$bin.new" && mv -f "$bin.new" "$bin"
installed || { say "installed binary does not report version $want"; exit 1; }
say "installed $want. Connect your agents with prefix + $(tmux show-option -gqv @tmag-key-setup 2>/dev/null || echo M-a)"
