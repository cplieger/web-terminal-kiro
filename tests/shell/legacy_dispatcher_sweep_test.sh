#!/usr/bin/env bash
# sweep_legacy_dispatchers(): a root `rm -rf "$dir/kiro-cli"*` on the volume's
# $HOME/.local/bin. The symlink case carries the weight: the glob expands
# through a symlinked dir, so the bait is a kiro-cli* entry in the link's target.
# SC2015: lib.sh's ok/no return 0 unconditionally, so `[ c ] && ok || no` cannot
# mis-fire.
# shellcheck disable=SC2015
set -u

# shellcheck source-path=SCRIPTDIR
. "$(dirname -- "$0")/lib.sh"
new_workdir >/dev/null

load_function logfmt_value
load_function sweep_legacy_dispatchers

# --- 1. ordinary sweep: dispatcher files and dirs go, other binaries stay -------
BIN="$WORK/home/.local/bin"
mkdir -p "$BIN/kiro-cli-chat.d"
: >"$BIN/kiro-cli"
: >"$BIN/kiro-cli-chat"
: >"$BIN/uv"
[ -f "$BIN/kiro-cli" ] && [ -d "$BIN/kiro-cli-chat.d" ] || {
  printf 'harness error: case 1 fixture missing\n' >&2
  exit 1
}
sweep_legacy_dispatchers "$BIN" 2>/dev/null
rc=$?
[ "$rc" -eq 0 ] && ok "ordinary sweep returns 0" || no "ordinary sweep" "returned $rc"
[ ! -e "$BIN/kiro-cli" ] && [ ! -e "$BIN/kiro-cli-chat" ] && [ ! -e "$BIN/kiro-cli-chat.d" ] \
  && ok "kiro-cli* files and directories removed" || no "ordinary sweep" "a kiro-cli* entry survived"
[ -e "$BIN/uv" ] && ok "a non-kiro-cli binary is left alone" || no "ordinary sweep" "uv was deleted"

# --- 2. THE SECURITY CASE: a symlinked dir must not redirect the root rm -rf ----
VICTIM="$WORK/victim"
mkdir -p "$VICTIM" "$WORK/home2/.local"
: >"$VICTIM/kiro-cli-victim"
ln -s "$VICTIM" "$WORK/home2/.local/bin"
[ -L "$WORK/home2/.local/bin" ] && [ -f "$VICTIM/kiro-cli-victim" ] || {
  printf 'harness error: case 2 fixture missing\n' >&2
  exit 1
}
err=$(sweep_legacy_dispatchers "$WORK/home2/.local/bin" 2>&1)
rc=$?
[ "$rc" -eq 0 ] && ok "symlinked dir returns 0 (hygiene, never fatal)" || no "symlinked dir" "returned $rc"
[ -f "$VICTIM/kiro-cli-victim" ] && ok "symlinked dir: the link target's kiro-cli* entry survives" \
  || no "symlinked dir" "the sweep deleted through the symlink"
case "$err" in
  *'level=warn msg="legacy kiro-cli residue dir is a symlink or not a directory; refusing to sweep through it"'*)
    ok "symlinked dir: the refusal is logged"
    ;;
  *) no "symlinked dir refusal" "missing warning: $err" ;;
esac

# --- 3. a plain file at the dir's name is refused, not swept beside ------------
: >"$WORK/notadir"
: >"$WORK/notadir-kiro-cli"
err=$(sweep_legacy_dispatchers "$WORK/notadir" 2>&1)
rc=$?
[ "$rc" -eq 0 ] && ok "non-directory returns 0" || no "non-directory" "returned $rc"
case "$err" in
  *'refusing to sweep through it'*) ok "non-directory: the refusal is logged" ;;
  *) no "non-directory refusal" "missing warning: $err" ;;
esac

# --- 4. an empty argument never expands to /kiro-cli* -------------------------
err=$(sweep_legacy_dispatchers "" 2>&1)
rc=$?
[ "$rc" -eq 0 ] && [ -z "$err" ] && ok "empty argument is a silent no-op" \
  || no "empty argument" "rc=$rc output: $err"

report
