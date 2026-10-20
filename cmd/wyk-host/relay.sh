#!/bin/sh
# Embedded in wyk-host.exe, which runs it in the distro with
# wsl.exe --exec sh -c <this script> sh <args>, the embedded wyk-relay on
# stdin. It runs the relay's own install or uninstall, which does the work.
# Both use this copy, not one already installed: an uninstall that failed
# after removing it can still be rerun.
#
# Values come in as arguments, never into the script text:
#   $1 wyk-host.exe as a Windows path   $2 the machine the relay is built for
#   $3 the version both must have       $4 the subcommand; the rest its flags
set -eu
m=$(uname -m)
[ "$m" = "$2" ] || { echo "wyk-host: this build is for $2, the distro runs $m" >&2; exit 1; }
exe=$1 ver=$3 cmd=$4
shift 4
if [ "$cmd" = install ]; then
  host=$(wslpath -u "$exe" 2>/dev/null) && [ -e "$host" ] ||
    { printf 'wyk-host: the distro cannot see %s; run it from a local drive\n' "$exe" >&2; exit 1; }
  [ -r "$host" ] || { printf 'wyk-host: %s cannot read %s\n' "$(id -un)" "$host" >&2; exit 1; }
  set -- -host "$host" "$@"
  mkdir -p "$HOME/.local/bin"
fi

# Staged where it is installed to, so a noexec /tmp cannot get in the way.
stage=$HOME/.local/bin
[ -d "$stage" ] || stage=$HOME # uninstall leaves no new folder behind
d=$(mktemp -d "$stage/.wyk-install.XXXXXX")
trap 'rm -rf "$d"' EXIT
trap 'exit 1' HUP INT TERM # sh runs the EXIT trap on exit only
cat >"$d/wyk-relay"
chmod 700 "$d/wyk-relay"
v=$("$d/wyk-relay" version)
[ "$v" = "$ver" ] || { echo "wyk-host: embedded wyk-relay is $v, this exe is $ver; build with scripts/build.sh" >&2; exit 1; }

# The relay runs in the user's login shell, so GNUPGHOME or SSH_AUTH_SOCK
# set in its login files (~/.profile, ~/.bash_profile, ~/.zprofile) count as
# in the user's own shells. A profile that execs another shell (exec zsh,
# exec tmux) would end it before the relay runs, and exit 0: the status file
# tells. It is written even if a profile turned on set -e.
# shellcheck disable=SC2016 # expanded by the login shell, not here
report='"$@" && s=0 || s=$?; echo $s >"$0"'
login=sh
case ${SHELL:-} in
*/bash | */zsh | */ksh) if [ -x "$SHELL" ]; then login=$SHELL; fi ;;
esac
if [ "$login" != sh ]; then
  "$login" -lc "$report" "$d/status" "$d/wyk-relay" "$cmd" "$@" </dev/null || :
else
  # sh, dash (which before 0.5.11 has no -l), another syntax like fish, or
  # a shell that is gone: read what sh -l would.
  # shellcheck disable=SC2016 # expanded by that sh, not here
  sh -c '[ ! -r /etc/profile ] || . /etc/profile; [ ! -r "$HOME/.profile" ] || . "$HOME/.profile"; '"$report" \
    "$d/status" "$d/wyk-relay" "$cmd" "$@" </dev/null || :
fi
[ -s "$d/status" ] || {
  echo "wyk-host: wyk-relay did not report back from your login shell ($login); does a profile exec another program?" >&2
  echo "wyk-host: fix that and run this again; uninstall also reverts a partial install" >&2
  exit 1
}
exit "$(cat "$d/status")"
