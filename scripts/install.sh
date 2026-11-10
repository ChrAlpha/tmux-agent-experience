#!/usr/bin/env bash
# Install only the checksum-verified release matching this checkout.
# Downloads have curl deadlines. Local tools/filesystems have no total cutoff.
set -u
set -o pipefail
[ "$#" -eq 1 ] || exit 2
valid_version='^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?(\+[0-9A-Za-z.-]+)?$'
[[ "$1" =~ $valid_version ]] || exit 2
want="$1"
dir="$(cd -P "${BASH_SOURCE[0]%/*}/.." && pwd -P)" || exit 1
bin="$dir/bin/tmag"
lock="$dir/bin/.install.lock"
# Keep failure reporting independent of tmux/client availability.
fail() { printf 'tmag installer: %s\n' "$*" >&2; exit 1; }
# A version prints "tmag VERSION (SCHEMA)"; the version decides the schema.
installed() { local v; v="$("$bin" version 2>/dev/null)"; [ "${v%% (*}" = "tmag $want" ]; }

# This script is always scheduled in the background by tmag.tmux. Even a stuck
# installed-version probe therefore cannot block tmux's plugin entry path.
[ ! -L "$dir/bin" ] || fail 'bin is a symlink; preserve it for owner inspection'
if [ -e "$bin" ] || [ -L "$bin" ]; then
 [[ -f "$bin" && ! -L "$bin" && -O "$bin" ]] || fail 'bin/tmag is not an owned regular file'
 installed && exit 0
fi
command -v curl >/dev/null 2>&1 || fail 'curl is required to download released binaries'
umask 077
mkdir -p "$dir/bin" || fail 'cannot create bin'
[[ -d "$dir/bin" && -O "$dir/bin" && ! -L "$dir/bin" ]] || fail 'bin ownership is uncertain'
mkdir "$lock" 2>/dev/null || fail 'install lock exists; after all installers finish, inspect bin/.install.lock and recover it from your own terminal'
# Cwd pins our exclusive directory without depending on /dev/fd stat semantics.
# Cooperating installers never remove an existing lock, regardless of its age.
cd -P "$lock" || fail 'cannot enter install lock; owner recovery is required'
[[ . -ef "$lock" && -O . ]] || fail 'install lock was replaced'
created_archive=0
created_checksums=0
created_candidate=0
cleanup_depth="$BASH_SUBSHELL"
cleanup() {
 # An inherited EXIT trap in a substitution does not own this workspace.
 [ "$BASH_SUBSHELL" = "$cleanup_depth" ] || return 0
 if [ ! -L "$lock" ] && [ . -ef "$lock" ]; then
  [ "$created_archive" = 0 ] || rm -f archive
  [ "$created_checksums" = 0 ] || rm -f checksums
  [ "$created_candidate" = 0 ] || rm -f candidate
  rmdir "$lock" 2>/dev/null || printf 'tmag installer: lock contains unknown residue; preserve it for owner inspection\n' >&2
 fi
}
trap cleanup EXIT
trap 'exit 1' HUP INT TERM
for name in archive checksums candidate; do
 (set -C; : >"$name") || fail 'unexpected object in install workspace'
 case "$name" in archive) created_archive=1 ;; checksums) created_checksums=1 ;; candidate) created_candidate=1 ;; esac
done
# Recheck after exclusion: a preceding installer may have completed already.
installed && exit 0
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
# Never extract archive pathnames: only one regular root member can be executed.
tar -tzf archive | awk '$0 == "tmag" { n++; next } $0 == "LICENSE" || $0 == "README.md" { next } { bad=1 } END { if (bad || n != 1) exit 1 }' || fail 'unexpected archive members'
tar -tvzf archive tmag | awk 'substr($0,1,1) != "-" { bad=1 } END { if (NR != 1 || bad) exit 1 }' || fail 'release binary is not regular'
tar -xOzf archive tmag >candidate || fail 'could not read released binary'
chmod 755 candidate || fail 'cannot mark released binary executable'
reported="$(./candidate version 2>/dev/null)"
[ "${reported%% (*}" = "tmag $want" ] || fail 'release binary reports a different version'
# The lock/workspace and destination share bin's filesystem. Refuse a rebound
# namespace; never delete another owner's lock or use a cross-filesystem copy.
[[ ! -L "$lock" && . -ef "$lock" && .. -ef "$dir/bin" && ! -L "$dir/bin" ]] || fail 'installation namespace changed'
[ ! -L "$bin" ] || fail 'binary destination became a symlink'
if [ -e "$bin" ]; then [[ -f "$bin" && -O "$bin" ]] || fail 'binary destination ownership changed'; fi
mv -f candidate ../tmag || fail 'could not publish released binary'
printf 'tmag installer: installed %s\n' "$want"
