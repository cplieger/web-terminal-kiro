#!/usr/bin/env bash
# warn_if_tools_noexec(): report a tools tree the container cannot execute from.
#
# The tree is proved writable by a create+remove probe at the call site, and a mode
# check proves it is private, but neither can see a noexec mount -- and the install
# manager execs the upstream installer out of a staging tree inside it. So this
# function's whole value is the noexec case, which is also the one no mode fixture
# can stage: it needs a real filesystem that refuses execve, discovered below.
#
# It is hygiene and diagnosis, never a gate: every case here also asserts it
# returns 0 and leaves no probe behind, because a warning that ends the boot or
# litters the volume would be worse than the four confusing lines it replaces.
#
# COVERAGE GAP, stated rather than papered over: all three assertions that can tell
# this function from an empty body live inside the `if NOEXEC=` block, so on a host
# that mounts no writable noexec tmpfs this file passes an empty function. Measured
# by mutation against /tmp copies (`ENTRYPOINT=/tmp/mut.sh`): dropping the hint, the
# errno, the probe removal or the warn-not-fatal posture is each caught by the
# assertion naming it, while an empty body reports 6 passed / 3 failed and every
# failure is in that block. Closing it needs a filesystem that refuses execve on
# demand, which `unshare -Urm` cannot provide without CAP_SYS_ADMIN. A second
# mechanism that stubbed `mktemp` into an unusable path would report the same three
# assertions off a fault the function does not exist for, so the skip is the honest
# answer: read the skip line as "this run proved less", not as a pass.
# Lint directives for this whole file, each against a stated guarantee rather than
# an assumption:
#   SC2015 - the assertion form `[ cond ] && ok "..." || no "..."` cannot mis-fire,
#     because lib.sh's ok/no return 0 unconditionally by design (see their comment).
# shellcheck disable=SC2015
set -u

# shellcheck source-path=SCRIPTDIR
. "$(dirname -- "$0")/lib.sh"
new_workdir >/dev/null

load_function logfmt_value
load_function warn_if_tools_noexec

# The one case that matters needs a filesystem that refuses execve, and no fixture
# can fake one: exec permission is a MOUNT property, so a mode, an ACL or an
# ownership change cannot stand in for it. The probe therefore runs in the
# mountpoint ITSELF rather than in a scratch dir beneath it, so this file creates
# nothing it could leak into a shared host tmpfs: lib.sh's EXIT trap owns $WORK
# only, and a second trap here would REPLACE it along with its forgot-report guard
# (see new_workdir), leaving a directory outside $WORK unowned on an aborted run.
# The only artifact is the function's own probe file, which the function removes and
# the assertions below check for.
#
# Restricted to tmpfs so the search never writes into a special filesystem that
# accepts mkdir with other meaning (cgroup2 is rw+noexec and a mkdir there
# creates a cgroup).
noexec_mountpoint() {
  local mp fstype opts probe
  while read -r _ mp fstype opts _; do
    [ "$fstype" = tmpfs ] || continue
    [ -d "$mp" ] || continue
    case ",$opts," in
      *,noexec,*) ;;
      *) continue ;;
    esac
    # Writability is proved, not inferred: on an unwritable tree the function
    # returns 0 in silence, and every assertion below would then fail for a
    # reason that has nothing to do with execution.
    probe=$(mktemp "$mp/.wtk-writable.XXXXXX" 2>/dev/null) || continue
    rm -f "$probe"
    printf '%s' "$mp"
    return 0
  done </proc/mounts
  return 1
}

probes_left() {
  set -- "$1"/.exec-probe.*
  [ -e "$1" ]
}

# --- 1. an executable tree is silent -------------------------------------------
# The precondition for every negative case: if the probe itself were broken (an
# unwritable dir, a chmod that fails), the noexec case below would warn for the
# wrong reason and still read as a pass.
GOOD="$WORK/good"
mkdir -p "$GOOD"
rc=0
warn_if_tools_noexec "$GOOD" >"$WORK/out.log" 2>"$WORK/warn.log" || rc=$?
[ "$rc" -eq 0 ] && ok "an executable tools tree returns 0" \
  || no "executable tree returns 0" "returned $rc"
[ ! -s "$WORK/warn.log" ] && ok "an executable tools tree warns about nothing" \
  || no "executable tree is silent" "warned: $(cat "$WORK/warn.log")"
probes_left "$GOOD" && no "probe removed" "an .exec-probe file was left in the tree" \
  || ok "the probe is removed on the silent path"

# --- 2. THE CASE THIS EXISTS FOR: a noexec tree is named ------------------------
if NOEXEC=$(noexec_mountpoint); then
  rc=0
  warn_if_tools_noexec "$NOEXEC" >"$WORK/out.log" 2>"$WORK/warn.log" || rc=$?
  grep -q 'cannot execute a file this script just created' "$WORK/warn.log" \
    && ok "a noexec tools tree is reported" \
    || no "noexec tree reported" "no warning naming the refused execution: $(cat "$WORK/warn.log")"
  # The message is the deliverable, so the assertion names the part that carries
  # the diagnosis. Without this, dropping the hint leaves the case green while
  # the operator is back to guessing at four unrelated log lines.
  grep -q 'mounted noexec' "$WORK/warn.log" \
    && ok "the warning names noexec as the cause to check" \
    || no "warning names noexec" "the hint is missing: $(cat "$WORK/warn.log")"
  # The captured interpreter message is what separates a noexec mount from a
  # mangled binary, so the field has to reach the line rather than stderr.
  grep -q 'error="[^"]*ermission denied' "$WORK/warn.log" \
    && ok "the warning carries the errno the interpreter reported" \
    || no "warning carries the errno" "no error= field: $(cat "$WORK/warn.log")"
  [ ! -s "$WORK/out.log" ] && ok "nothing leaks onto stdout" \
    || no "stdout is clean" "wrote: $(cat "$WORK/out.log")"
  [ "$rc" -eq 0 ] && ok "a noexec tools tree still returns 0 (warn, never fatal)" \
    || no "noexec tree returns 0" "returned $rc, which would end the boot"
  probes_left "$NOEXEC" && no "probe removed" "an .exec-probe file was left in the tree" \
    || ok "the probe is removed on the warning path too"
else
  skip "noexec tools tree is reported" "this host mounts no writable noexec tmpfs to stage the case on"
fi

# --- 3. an unwritable tree stays silent ----------------------------------------
# The call site's write probe is fatal and runs FIRST, so by the time this function
# runs an unwritable tree has already ended the boot. Warning here would only
# misattribute that failure to execution.
UNWRITABLE="$WORK/unwritable"
mkdir -p "$UNWRITABLE"
chmod 500 "$UNWRITABLE"
rc=0
warn_if_tools_noexec "$UNWRITABLE" >"$WORK/out.log" 2>"$WORK/warn.log" || rc=$?
chmod 700 "$UNWRITABLE"
if [ "$(id -u)" -eq 0 ]; then
  skip "an unwritable tools tree stays silent" "root writes through a 0500 directory, so the premise cannot hold here"
else
  [ "$rc" -eq 0 ] && [ ! -s "$WORK/warn.log" ] \
    && ok "an unwritable tools tree stays silent (the write probe owns that failure)" \
    || no "unwritable tree stays silent" "returned $rc and warned: $(cat "$WORK/warn.log")"
fi

# --- 4. the probe runs only on an already-tightened tree ------------------------
# An ORDERING invariant, so it is read out of the shipped script rather than
# executed: the probe is created, chmod 700'd and then EXECUTED AS ROOT, so on the
# group/other-writable volume the taint flag exists for, running it before
# secure_tools_dir has stripped those bits lets a foreign host user unlink it and
# substitute a payload between the chmod and the exec. No single run can expose the
# order the two call sites appear in.
probe_line=$(grep -n '^warn_if_tools_noexec ' "$ENTRYPOINT" | head -1 | cut -d: -f1)
secure_line=$(grep -n '^secure_tools_dir "[$]TOOLS"$' "$ENTRYPOINT" | head -1 | cut -d: -f1)
if [ -z "$probe_line" ] || [ -z "$secure_line" ]; then
  no "the exec probe runs after the tree is tightened" \
    "could not find both call sites in $ENTRYPOINT (probe=${probe_line:-none} secure=${secure_line:-none})"
else
  [ "$probe_line" -gt "$secure_line" ] \
    && ok "the exec probe is called after secure_tools_dir stripped group/other write bits" \
    || no "the exec probe runs after the tree is tightened" \
      "warn_if_tools_noexec is called at line $probe_line, above secure_tools_dir at line $secure_line"
fi

report
