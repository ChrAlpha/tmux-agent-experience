#!/usr/bin/env bash
# Install the checksum-verified release matching this checkout into bin/.
# Each run downloads into a directory of its own and publishes the binary
# with one rename: runs at once each publish the same verified release, and a
# run that dies leaves only its directory, which a later run removes.
# Downloads have curl deadlines.
set -u
set -o pipefail
[ "$#" -eq 1 ] || exit 2
valid_version='^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?(\+[0-9A-Za-z.-]+)?$'
[[ "$1" =~ $valid_version ]] || exit 2
want="$1"
dir="$(cd -P "${BASH_SOURCE[0]%/*}/.." && pwd -P)" || exit 1
bin="$dir/bin/tmag"
# Keep failure reporting independent of tmux/client availability.
fail() { printf 'tmag installer: %s\n' "$*" >&2; exit 1; }
# A version prints "tmag VERSION (SCHEMA)"; the version decides the schema.
reports() { local v; v="$("$1" version 2>/dev/null)"; [ "${v%% (*}" = "tmag $want" ]; }

# tmag.tmux schedules this in the background, so even a binary that hangs on
# version cannot block tmux's plugin entry point.
reports "$bin" && exit 0
command -v curl >/dev/null 2>&1 || fail 'curl is required to download released binaries'
umask 077
mkdir -p "$dir/bin" || fail 'cannot create bin'
# What runs that died left; a run in progress is younger.
find "$dir/bin" -maxdepth 1 -name '.install.*' -mmin +10 -exec rm -rf {} + 2>/dev/null
work="$(mktemp -d "$dir/bin/.install.XXXXXX")" || fail 'cannot create a work directory in bin'
trap 'rm -rf "$work"' EXIT
trap 'exit 1' HUP INT TERM
cd "$work" || fail 'cannot enter the work directory'
os="$(uname -s)" || fail 'cannot identify platform'
case "$os" in Linux) os=linux ;; Darwin) os=darwin ;; *) fail 'unsupported platform' ;; esac
case "$(uname -m)" in x86_64 | amd64) arch=amd64 ;; aarch64 | arm64) arch=arm64 ;; *) fail 'unsupported architecture' ;; esac
asset="tmag_${want}_${os}_${arch}.tar.gz"
base="$(tmux show-option -gqv @tmag-release-url 2>/dev/null)"
base="${base:-https://github.com/ChrAlpha/tmux-agent-experience/releases/download}"
fetch() {
 curl -q -fsSL --connect-timeout 10 --max-time 25 --retry 0 --max-redirs 5 -o "$2" -- "$1"
}
fetch "$base/v$want/$asset" archive || fail "could not download release $want"
fetch "$base/v$want/checksums.txt" checksums || fail 'could not download release checksums'
expected="$(awk -v name="$asset" '
 NF == 2 && $2 == name { n++; if (length($1) != 64 || $1 !~ /^[[:xdigit:]]+$/) bad=1; hash=tolower($1) }
 END { if (n != 1 || bad) exit 1; print hash }
' checksums)" || fail 'missing or ambiguous release checksum'
if command -v sha256sum >/dev/null 2>&1; then actual="$(sha256sum archive)"; else actual="$(shasum -a 256 archive)"; fi
[ "${actual%% *}" = "$expected" ] || fail 'release checksum mismatch'
# Only the archive's tmag is read, never a path it names.
tar -xOzf archive tmag >candidate || fail 'could not read released binary'
chmod 755 candidate || fail 'cannot mark released binary executable'
reports ./candidate || fail 'release binary reports a different version'
mv -f candidate "$bin" || fail 'could not publish released binary'
printf 'tmag installer: installed %s\n' "$want"
