# Command sandbox

This page explains what the two sandbox profiles allow and why, how the startup check decides whether kiro-cli gets its sandbox, and what changes for a command that runs inside it. The README section [Turning on the command sandbox](../README.md#turning-on-the-command-sandbox) has the setup steps.

## What the sandbox is

kiro-cli's `/sandbox` setting runs each command the agent starts inside [bubblewrap](https://github.com/containers/bubblewrap), whose command is `bwrap`. bubblewrap gives the command its own mount and process namespaces, and by default its own network namespace. Inside them the system is read-only, the workspace and `~/.kiro` stay writable, a fresh `/tmp` is empty, and kiro-cli hides credentials such as `~/.ssh`, `~/.config/gh` and AWS keys, plus shell histories.

kiro-cli does the sandboxing. Web Terminal for Kiro ships bubblewrap and `socat`, the profiles that let them work inside a container, and the startup check below.

## Why Docker blocks it

The server and every tab run as root, without the `CAP_SYS_ADMIN` capability. bubblewrap then needs a user namespace, which the image asks for on every `bwrap` call. That namespace gives bubblewrap the capabilities it needs over its new namespaces and nothing outside them. Docker's default profiles refuse every step:

- The default seccomp profile refuses `clone` with any namespace flag, and refuses `mount`, `umount2` and `pivot_root` without `CAP_SYS_ADMIN`. The [Docker seccomp docs](https://docs.docker.com/engine/security/seccomp/) list each one.
- The default AppArmor profile, `docker-default`, denies every mount. The [Docker AppArmor docs](https://docs.docker.com/engine/security/apparmor/) describe it.

Under those defaults bubblewrap fails with `Creating new namespace failed: Operation not permitted`.

## What the seccomp profile adds

[`sandbox/seccomp.json`](../sandbox/seccomp.json) is Docker's default profile from [moby/profiles](https://github.com/moby/profiles/blob/6fe7deb1b9fb7c0397a4593480d7d22b9ee8caef/seccomp/default.json), unchanged except for three rules placed after Docker's own `clone` rules. Each carries a `comment` field.

| Rule | Why bubblewrap needs it |
| --- | --- |
| `clone` when the namespace flags are exactly new mount, user, process and network namespaces | Every command in kiro-cli's "block all" and "common dependencies" network modes, and the startup check |
| `clone` when the namespace flags are exactly new mount, user and process namespaces | Every command in kiro-cli's "allow all" network mode, which keeps the container's network |
| `mount`, `umount2`, `pivot_root` | Building the command's view of the file system inside its own mount namespace, then switching to it |

The `mount`, `umount2` and `pivot_root` rules do not check their arguments. The AppArmor profile below is what limits them to the mounts bubblewrap makes, so the seccomp profile is never used without it, and the sandbox needs a host with AppArmor.

The `clone` rules match the namespace flags exactly, so a process cannot also create new IPC, host name or cgroup namespaces. `unshare`, `setns` and `clone3` stay refused, and so do the newer mount calls `mount_setattr`, `open_tree`, `move_mount`, `fsopen`, `fsmount` and `fspick`, because bubblewrap does not use them here. On s390 and s390x the added `clone` rules do not apply.

## What the AppArmor profile adds

[`sandbox/web-terminal-kiro-sandbox`](../sandbox/web-terminal-kiro-sandbox) is Docker's `docker-default` profile, rendered from [moby/profiles](https://github.com/moby/profiles/blob/a21872828a8e5745d79e2fc1a07ee6dad8aedffa/apparmor/template.go), with its `deny mount,` replaced by the mounts bubblewrap makes:

| Rule | Why bubblewrap needs it |
| --- | --- |
| `mount options in (rslave, silent) -> /,` | Stops mounts inside the sandbox from reaching the container |
| `mount fstype=tmpfs options in (nodev, nosuid, silent) -> /**,` | The new root, `/tmp`, `/run`, `/dev` and the empty `/proc` |
| `mount fstype=devpts options in (noexec, nosuid, silent) -> /**,` | `/dev/pts` inside the new `/dev` |
| `mount options in (bind, rbind, silent) -> /**,` | The read-only system, the writable workspace and `~/.kiro`, and the device files |
| `mount options in (rprivate, silent) -> /**,` | Detaching the old root without sending events back to the container |
| `remount /**,` | Making the system read-only and adding `nosuid` and `nodev`. It cannot loosen a mount inherited from the container, as [mount_namespaces(7)](https://man7.org/linux/man-pages/man7/mount_namespaces.7.html) describes |
| `pivot_root,` | Switching to the new root |
| `deny mount fstype=proc,` | Refuses a fresh `/proc` without logging it. Docker hides parts of `/proc`, so a fresh one cannot mount there anyway |

The profile keeps Docker's `abi <abi/3.0>`, so it loads on hosts with AppArmor 3.0 or later.

With both profiles, any process in the container can create its own user namespace and mount file systems inside it, which Docker's defaults refuse. Docker reads the seccomp file on the machine you run `docker compose` from, so `security_opt` needs its full path there.

## The startup check

With `KIRO_CLI_SANDBOX=true`, the server checks the sandbox once at each container start, before it accepts connections, and keeps that answer until the next start. It does not watch the host afterwards. AppArmor applies a replaced profile, a changed mode or a removed profile to the running container at once, so restart the container after any of these to check again. If the profile was removed, Docker refuses to start the container until you load the profile again. Docker stores the seccomp file's content in the container's configuration when it creates the container, so a changed seccomp file needs the container recreated, for example with `docker compose up -d --force-recreate`.

1. It reads the container's AppArmor confinement from `/proc/self/attr/apparmor/current`, or `/proc/self/attr/current` on kernels older than 5.8. Unless it names the `web-terminal-kiro-sandbox` profile in enforce mode, the sandbox is unavailable and no command runs. Docker ignores `apparmor=web-terminal-kiro-sandbox` on a host without AppArmor, and the seccomp profile's `mount` rules are then limited by nothing.
2. It runs kiro-cli's own bubblewrap probe, which asks for a fresh `/proc`. The image adds a PID namespace to every `bwrap` call, as kiro-cli's commands already have, so the probe tests the same `/proc` mount those commands need.
3. It runs `/bin/true` with the namespace and mount arguments kiro-cli uses for a command in "block all" mode. Like kiro-cli, it asks for a fresh `/proc` if the probe worked and an empty one if it did not.
4. If that command ran, the sandbox is available. If it failed or took more than 5 seconds, the sandbox is unavailable.

The image keeps bubblewrap off `PATH`. With the setting on, the server puts a folder holding only a `bwrap` first on each tab's `PATH`, so kiro-cli finds that one before any other. When the check passed, that `bwrap` is bubblewrap. When it failed, that `bwrap` refuses every command, and kiro-cli reports its sandbox as unavailable.

kiro-cli then says the system `bwrap` could not be loaded and suggests reinstalling it. In this image that message means the startup check failed, and the server log says why. This matters because kiro-cli reports its sandbox as on when its `bwrap` fails in most other ways, so a sandbox that cannot start would look on. That is [kirodotdev/Kiro#11941](https://github.com/kirodotdev/Kiro/issues/11941).

`/api/health` carries the result in its `sandbox` field, on both 200 and 503 answers. It never decides whether the instance is healthy.

| `sandbox` | Meaning |
| --- | --- |
| `off` | `KIRO_CLI_SANDBOX` is unset or off. No check ran. |
| `available` | A sandboxed command ran. The log shows `kiro-cli sandbox available`. |
| `unavailable` | The container is not under the `web-terminal-kiro-sandbox` profile in enforce mode, or the sandboxed command failed. The log warns `kiro-cli sandbox unavailable` with the confinement it found or bubblewrap's own error. |

With the setting off, tabs keep their `PATH` as it is. The server then warns at start when a `bwrap` is on its `PATH`, for example one installed through the tools list, because kiro-cli would find that one and could report a sandbox that cannot run. A `bwrap` installed after the start is not checked. With the setting on, tabs never find such a `bwrap`, even one installed later.

## Inside the sandbox

- Commands run as root inside their own user namespace, which maps only root. A workspace file owned by another host user shows as owned by `nobody`, and the command can change it only through its group or other write permission.
- `/proc` inside the sandbox is empty, because Docker's hidden `/proc` paths stop a fresh one from mounting. Tools that read `/proc`, such as `ps`, see nothing.
- In "block all" mode the command has its own network with only a loopback device. In "common dependencies" mode kiro-cli runs a local proxy with an allowlist, through `socat` and a socket under `/run/user/0`, which the server creates when the sandbox is available. In "allow all" mode the command uses the container's network.
- The profiles work with `security_opt: ["no-new-privileges"]` and with `cap_add: [SYS_ADMIN]`. With `SYS_ADMIN` granted, the processes that keep it, the container's init and `docker exec` shells, can also make the mounts above in the container itself, which `docker-default` would refuse.
