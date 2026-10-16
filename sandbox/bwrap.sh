#!/bin/sh
# bwrap creates a user namespace on its own only for a non-root caller
# (bubblewrap.c: `if (getuid () != 0 ...) opt_unshare_user = true`). Tabs run
# as root without CAP_SYS_ADMIN, so without --unshare-user every namespace it
# creates is refused. kiro-cli never passes the flag itself.
# kiro-cli's probe omits --unshare-pid, so bwrap bind-mounts the container's
# /proc for its --proc (bubblewrap.c SETUP_MOUNT_PROC) and can pass while every
# command, which unshares pid and needs a fresh /proc, fails.
exec /opt/web-terminal-kiro/libexec/bwrap --unshare-user --unshare-pid "$@"
