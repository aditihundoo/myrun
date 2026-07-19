# myrun

A toy container runtime built from scratch in Go, to understand what
`docker run` actually does underneath: Linux namespaces, chroot, cgroups,
and (eventually) container networking.

This is **not** meant to replace Docker -- it's a from-scratch
reimplementation of the core primitives, for learning and portfolio
purposes.

## Status

- [x] **Milestone 1: Process isolation** -- PID/UTS/mount/IPC namespaces
      via `clone` flags, chroot into a rootfs, fresh `/proc` mount
- [x] **Milestone 2: Resource limits** -- cgroups v2 for CPU/memory caps
      via `--memory`/`--cpu` flags. **Written against the real cgroups v2
      API but not yet verified against a real cgroup v2 host** -- it was
      developed in a sandbox whose `/sys/fs/cgroup` turned out to be a
      plain tmpfs with legacy v1 subdirectories rather than a true v2
      mount, so writes succeeded without actually enforcing anything.
      Verify on a real machine before relying on this: `mount | grep
      "on /sys/fs/cgroup type"` should say `cgroup2`, and a
      memory-limited container that exceeds its limit should get
      OOM-killed (exit code 137).
- [ ] **Milestone 3: Real OCI images** -- pull and unpack an image from
      Docker Hub instead of a hand-built rootfs
- [ ] **Milestone 4: Networking** -- veth pair + bridge for container
      network access

## How it works

Real container runtimes can't just call `unshare()` on the current
process and continue running -- some namespaces (like PID) only take
effect for *new* processes created after the namespace is entered, not
the calling process itself. So the pattern (also used by Docker/runc)
is:

1. `myrun run <rootfs> <cmd>` launches a **re-exec** of the same binary
   as `myrun child <rootfs> <cmd>`, via `os/exec` with
   `SysProcAttr.Cloneflags` set to `CLONE_NEWPID | CLONE_NEWUTS |
   CLONE_NEWNS | CLONE_NEWIPC`. This makes the *new* process the first
   process (PID 1) inside a fresh set of namespaces.
2. That child process (`internal/container.Child`) sets a container
   hostname, `chroot`s into the rootfs, mounts a fresh `/proc`, and
   runs the user's requested command.

## Usage

```sh
go build -o myrun ./cmd/myrun

# Build a minimal busybox-based test rootfs (no registry access needed)
./scripts/build-test-rootfs.sh

./myrun run "$(pwd)/rootfs-minimal" /bin/sh

# with resource limits:
./myrun run --memory=50m --cpu=0.5 "$(pwd)/rootfs-minimal" /bin/sh
```

Inside the shell, try `hostname`, `ps`, and `ls /` -- they'll show
container-local values, isolated from the host.

To verify the memory limit is actually enforced (see the Milestone 2
caveat above -- do this on a real machine, not a nested sandbox):

```sh
./myrun run --memory=20m "$(pwd)/rootfs-minimal" /bin/sh -c \
  'yes | tr \n x | head -c 200000000 | wc -c'
# should be OOM-killed (exit 137), not print 200000000
```

## Known limitations (Milestone 1)

- The runtime process itself becomes PID 1 inside the namespace, and
  the user's command runs as its child (PID 2+), rather than the user's
  command being PID 1 directly. Real runtimes exec directly into the
  target command to avoid this. Candidate fix: use `syscall.Exec`
  instead of `exec.Command(...).Run()` in the child.
- cgroups limits need verification on a real cgroup v2 host (see above)
- Rootfs must be pre-built locally; no image pulling yet (Milestone 3)
- Container shares the host's network namespace (Milestone 4)
- Requires root / appropriate capabilities to create namespaces

## Requirements

- Linux (namespaces are Linux-specific; won't run on macOS/Windows
  natively)
- Go 1.22+
- Root privileges (or `CAP_SYS_ADMIN`) to create namespaces
