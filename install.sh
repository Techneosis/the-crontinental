#!/usr/bin/env bash
# Install Crontinental for the current user.
#
#   curl -fsSL https://raw.githubusercontent.com/Techneosis/the-crontinental/main/install.sh | bash
#
# Run that way (or as a lone downloaded file) it fetches the latest release for
# this machine, checks its SHA-256, unpacks it into
# ~/.local/share/crontinental/dist/<version>/, and installs from there; running
# it again upgrades. Options go after `bash -s --`, e.g. `| bash -s -- --claude`.
# Run from a checkout, it builds from source instead, and links into the
# checkout so edits are live.
#
#   ./install.sh                link everything, start the tick, set up Claude Code
#   ./install.sh --version v0.1.0   that release rather than the latest (download only)
#   ./install.sh --claude       ...and add the CLAUDE.md section without asking
#   ./install.sh --no-claude    skip Claude Code entirely
#   ./install.sh --claude-only  only the Claude Code part (no binary, no tick)
#   ./install.sh --uninstall    remove all of it (the ledger stays)
#   ./install.sh --uninstall --claude-only   remove only the Claude Code part
#   ./install.sh --no-build     install build/crontinental as it is (a release
#                               binary, or one cross-compiled elsewhere)
#
# Linux: the tick is a systemd user timer. macOS: it is a LaunchAgent
# (~/Library/LaunchAgents/com.techneosis.crontinental.plist), and job output
# goes to ~/Library/Logs/crontinental/.
#
# Claude Code: when ~/.claude (or $CLAUDE_CONFIG_DIR) exists, the crontinental
# skill is linked into its skills/, and a short routing section is offered for
# its CLAUDE.md. That section sits between marker comments, so re-running
# updates it in place and --uninstall takes it out again. CLAUDE.md is written
# in place, never replaced, so a CLAUDE.md that is itself a symlink (dotfiles)
# stays one.
#
# Notifications are hooks in ~/.config/crontinental/notify.d/. A first install
# copies in a desktop hook (a copy, not a link: it is the user's to change);
# after that the directory is left alone, so one emptied on purpose stays
# empty, and --uninstall leaves it like the ledger.
#
# Paths are fixed: the tick runs ~/.local/bin/crontinental.
set -euo pipefail

repo=Techneosis/the-crontinental
releases=${CRONTINENTAL_RELEASES:-https://github.com/$repo/releases}
data=${XDG_DATA_HOME:-$HOME/.local/share}/crontinental

# Where this script runs from decides what it installs. Piped into bash it has
# no file at all, and the working directory says nothing about it.
if [[ -n ${BASH_SOURCE[0]:-} && -f ${BASH_SOURCE[0]} ]]; then
  src=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
else
  src=""
fi
if [[ -n $src && -f $src/cmd/crontinental/main.go ]]; then
  mode=checkout
elif [[ -n $src && -f $src/VERSION && -x $src/build/crontinental ]]; then
  mode=release
else
  mode=download
fi
bin=$HOME/.local/bin
units=${XDG_CONFIG_HOME:-$HOME/.config}/systemd/user
comp=${XDG_DATA_HOME:-$HOME/.local/share}/bash-completion/completions
hooks=${XDG_CONFIG_HOME:-$HOME/.config}/crontinental/notify.d
claude=${CLAUDE_CONFIG_DIR:-$HOME/.claude}

BEGIN='<!-- crontinental:begin (managed by crontinental/install.sh; edit agents/claude-md-snippet.md upstream, not here) -->'
END='<!-- crontinental:end -->'

uninstall=0 claude_only=0 claude_md=ask build=1 want_version=latest
[[ $mode == release ]] && build=0
os=$(uname -s)
label=com.techneosis.crontinental
agent=$HOME/Library/LaunchAgents/$label.plist
logdir=$HOME/Library/Logs/crontinental
passthrough=()
while (($#)); do
  arg=$1; shift
  case $arg in
    --version)     want_version=${1:?--version needs a release, e.g. v0.1.0}; shift; continue ;;
    --version=*)   want_version=${arg#*=}; continue ;;
    --uninstall)   uninstall=1 ;;
    --claude-only) claude_only=1 ;;
    --claude)      claude_md=yes ;;
    --no-claude)   claude_md=never ;;
    --no-build)    build=0 ;;
    -h|--help)     sed -n '2,36p' "${BASH_SOURCE[0]:-/dev/null}" 2>/dev/null | sed 's/^# \{0,1\}//'
                   [[ -z $src ]] && echo "see https://github.com/$repo#readme"; exit 0 ;;
    *) echo "unknown option: $arg (try --help)" >&2; exit 2 ;;
  esac
  passthrough+=("$arg")
done

# ---------------------------------------------------------------- download

platform() {
  local o a
  case $(uname -s) in Linux) o=linux ;; Darwin) o=darwin ;;
    *) echo "crontinental runs on Linux and macOS, not $(uname -s)" >&2; exit 1 ;; esac
  case $(uname -m) in x86_64|amd64) a=amd64 ;; aarch64|arm64) a=arm64 ;;
    *) echo "no crontinental build for $(uname -m)" >&2; exit 1 ;; esac
  echo "$o-$a"
}

sha256() { if command -v sha256sum >/dev/null; then sha256sum "$1"; else shasum -a 256 "$1"; fi | cut -d' ' -f1; }

download_and_install() {
  local plat asset url tmp want got ver
  plat=$(platform)
  asset=crontinental-$plat.tar.gz
  if [[ $want_version == latest ]]; then url=$releases/latest/download; else url=$releases/download/$want_version; fi
  tmp=$(mktemp -d)
  trap 'rm -rf "$tmp"' EXIT
  echo "fetching $asset ($want_version)"
  curl -fsSL "$url/$asset" -o "$tmp/$asset" || { echo "could not download $url/$asset" >&2; exit 1; }
  curl -fsSL "$url/SHA256SUMS" -o "$tmp/SHA256SUMS" || { echo "could not download $url/SHA256SUMS" >&2; exit 1; }
  want=$(awk -v a="$asset" '$2 == a || $2 == "*"a {print $1}' "$tmp/SHA256SUMS")
  got=$(sha256 "$tmp/$asset")
  if [[ -z $want || $want != "$got" ]]; then
    echo "checksum mismatch for $asset: expected ${want:-nothing}, got $got. Not installing." >&2
    exit 1
  fi
  echo "checksum ok"
  tar -xzf "$tmp/$asset" -C "$tmp"
  ver=$(cat "$tmp/crontinental-$plat/VERSION")
  mkdir -p "$data/dist"
  rm -rf "${data:?}/dist/$ver"
  mv "$tmp/crontinental-$plat" "$data/dist/$ver"
  # Everything links through dist/current, so an upgrade is one swapped link.
  local previous=""
  [[ -L $data/dist/current ]] && previous=$(readlink "$data/dist/current")
  ln -sfn "$ver" "$data/dist/current"
  # Keep this release and the one before it; older ones go.
  for d in "$data"/dist/*/; do
    d=$(basename "$d")
    [[ $d == current || $d == "$ver" || $d == "$previous" ]] || rm -rf "${data:?}/dist/$d"
  done
  echo "unpacked $ver into $data/dist/$ver"
  exec bash "$data/dist/current/install.sh" "${passthrough[@]+"${passthrough[@]}"}"
}

if [[ $mode == download && $uninstall == 0 ]]; then
  download_and_install
fi
if [[ $mode != download && $want_version != latest ]]; then
  echo "--version is for downloading a release; this is a $mode install" >&2
  exit 2
fi

links=(
  "$src/build/crontinental:$bin/crontinental"
  "$src/completions/crontinental.bash:$comp/crontinental"
)
if [[ $os == Linux ]]; then
  links+=(
    "$src/systemd/crontinental.service:$units/crontinental.service"
    "$src/systemd/crontinental.timer:$units/crontinental.timer"
  )
fi

link() {  # link <from> <to>, refusing to clobber anything that isn't a link
  local from=$1 dst=$2
  mkdir -p "$(dirname "$dst")"
  if [[ -e $dst && ! -L $dst ]]; then
    echo "refusing to replace $dst: it is a real file, not a link" >&2
    exit 1
  fi
  ln -sfn "$from" "$dst"
  echo "linked $dst"
}

# Rewrite a file through any symlink: content in place, never a rename.
rewrite() {  # rewrite <file> <awk program> [awk args...]
  local f=$1 prog=$2 tmp; shift 2
  tmp=$(mktemp)
  awk "$@" "$prog" "$f" >"$tmp" && cat "$tmp" >"$f"
  rm -f "$tmp"
}

has_block() { [[ -f $1 ]] && grep -qF '<!-- crontinental:begin' "$1"; }

claude_install() {
  [[ $claude_md == never ]] && return 0
  if [[ ! -d $claude ]]; then
    echo "Claude Code not found at $claude; skipping its setup (CLAUDE_CONFIG_DIR points elsewhere)"
    return 0
  fi
  link "$src/agents/skills/crontinental" "$claude/skills/crontinental"

  local md=$claude/CLAUDE.md
  if has_block "$md"; then
    rewrite "$md" '
      index($0, "<!-- crontinental:begin") == 1 { print b; while ((getline l < snip) > 0) print l; skip = 1; next }
      index($0, "<!-- crontinental:end") == 1   { skip = 0; print e; next }
      !skip' -v b="$BEGIN" -v e="$END" -v snip="$src/agents/claude-md-snippet.md"
    echo "updated the crontinental section of $md"
    return 0
  fi

  if [[ $claude_md == ask ]]; then
    # Piped from curl, stdin is the script; a person at a terminal still has
    # /dev/tty, and an unattended run has neither.
    local from=""
    if [[ -t 0 ]]; then from=/dev/stdin; elif [[ -t 1 ]] && { : </dev/tty; } 2>/dev/null; then from=/dev/tty; fi
    if [[ -n $from ]]; then
      echo
      echo "Add a short section to $md so Claude reaches for crontinental"
      echo "(rather than cron, a hand-written timer, or a session-scoped loop)?"
      read -r -p "[Y/n] " reply <"$from"
      [[ -z $reply || $reply == [Yy]* ]] || { echo "skipped; ./install.sh --claude adds it later"; return 0; }
    else
      echo "not adding a CLAUDE.md section without asking; run the installer again with --claude to add it"
      return 0
    fi
  fi
  { [[ -s $md ]] && echo; echo "$BEGIN"; cat "$src/agents/claude-md-snippet.md"; echo "$END"; } >>"$md"
  echo "added a crontinental section to $md"
}

claude_uninstall() {
  [[ -L $claude/skills/crontinental ]] && rm "$claude/skills/crontinental" && echo "removed $claude/skills/crontinental"
  if has_block "$claude/CLAUDE.md"; then
    # Also drops the one blank line install put before the section.
    rewrite "$claude/CLAUDE.md" '
      held { if (index($0, "<!-- crontinental:begin") != 1) print ""; held = 0 }
      index($0, "<!-- crontinental:begin") == 1 { skip = 1; next }
      index($0, "<!-- crontinental:end") == 1   { skip = 0; next }
      skip { next }
      $0 == "" { held = 1; next }
      { print }
      END { if (held) print "" }'
    echo "removed the crontinental section from $claude/CLAUDE.md"
  fi
}

case $uninstall$claude_only in
  01) claude_install; exit 0 ;;
  11) claude_uninstall; exit 0 ;;
  10)
    if [[ $os == Darwin ]]; then
      launchctl bootout "gui/$(id -u)/$label" 2>/dev/null || true
      [[ -f $agent ]] && rm "$agent" && echo "removed $agent"
    else
      systemctl --user disable --now crontinental.timer 2>/dev/null || true
    fi
    for pair in "${links[@]}"; do
      dst=${pair#*:}
      [[ -L $dst ]] && rm "$dst" && echo "removed $dst"
    done
    [[ $os == Linux ]] && systemctl --user daemon-reload
    claude_uninstall
    if [[ -d $data/dist ]]; then rm -rf "${data:?}/dist" && echo "removed $data/dist"; fi
    echo "uninstalled; the ledger is left at $data/ and the notify hooks at $hooks/"
    exit 0 ;;
esac

case $os in
  Linux)
    systemctl --user show-environment >/dev/null 2>&1 \
      || { echo "crontinental needs a running systemd user manager" >&2; exit 1; } ;;
  Darwin)
    command -v launchctl >/dev/null || { echo "launchctl not found" >&2; exit 1; } ;;
  *) echo "crontinental runs on Linux (systemd) and macOS (launchd), not $os" >&2; exit 1 ;;
esac

if ((build)); then
  command -v go >/dev/null \
    || { echo "building crontinental from a checkout needs Go (https://go.dev/dl/); or put a binary at build/crontinental and pass --no-build" >&2; exit 1; }
  # Build to a temp name and rename into place: a tick that starts mid-build
  # then runs the old binary or the new one, never half of one.
  (cd "$src" && CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o build/crontinental.new ./cmd/crontinental)
  mv -f "$src/build/crontinental.new" "$src/build/crontinental"
  echo "built $src/build/crontinental"
elif [[ ! -x $src/build/crontinental ]]; then
  echo "--no-build, but there is no binary at $src/build/crontinental" >&2
  exit 1
fi
"$src/build/crontinental" path >/dev/null \
  || { echo "$src/build/crontinental doesn't run here (built for another platform?)" >&2; exit 1; }

for pair in "${links[@]}"; do
  link "${pair%%:*}" "${pair#*:}"
done

if [[ ! -e $hooks ]]; then
  mkdir -p "$hooks"
  cp "$src/notify/desktop" "$hooks/desktop"
  echo "added a desktop notify hook: $hooks/desktop (more in $src/notify/examples/)"
fi

if [[ $os == Darwin ]]; then
  # The plist is written, not linked: launchd needs absolute paths in it.
  mkdir -p "$(dirname "$agent")" "$logdir"
  sed -e "s|@BIN@|$bin/crontinental|g" -e "s|@LOGDIR@|$logdir|g" \
    "$src/launchd/$label.plist" >"$agent"
  echo "wrote $agent"
  launchctl bootout "gui/$(id -u)/$label" 2>/dev/null || true
  launchctl bootstrap "gui/$(id -u)" "$agent"
  launchctl print "gui/$(id -u)/$label" >/dev/null 2>&1 \
    || { echo "launchd didn't take $agent" >&2; exit 1; }
  echo "the Concierge is on duty: loaded, every 60s (logs in $logdir)"
else
  systemctl --user daemon-reload
  systemctl --user enable --now crontinental.timer
  echo "the Concierge is on duty: $(systemctl --user is-active crontinental.timer)"
fi
[[ :$PATH: == *":$bin:"* ]] || echo "note: $bin is not on your PATH"

claude_install
