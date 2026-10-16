#!/bin/sh
# Tabs resolve this bwrap when the server's startup check failed. Exit 127
# without "bwrap: execvp " in the output is one of the probe failures kiro-cli
# classes as a bwrap that cannot load, which it reports as an unavailable
# sandbox. It reports most other probe failures as a sandbox that is on
# (https://github.com/kirodotdev/Kiro/issues/11941). kiro-cli 2.31.1, KAS
# LocalSandbox isAvailable.
printf '%s\n' 'web-terminal-kiro: the startup check found that the kiro-cli sandbox cannot run in this container; the server log says why' >&2
exit 127
