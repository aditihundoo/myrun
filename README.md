# myrun

A toy container runtime built from scratch in Go, to understand what
`docker run` actually does underneath: Linux namespaces, chroot,
cgroups, and pulling real images from a registry.

This is **not** meant to replace Docker -- it's a from-scratch
reimplementation of the core primitives, for learning and portfolio
purposes.

## Status

- [x] **Process isolation** -- PID/UTS/mount/IPC namespaces via `clone`
      flags, chroot into a rootfs, fresh `/proc` mount
- [x] **Resource limits** -- CPU/memory caps via `--memory`/`--cpu`
      flags, backed by cgroups. Auto-detects and supports both cgroups
      v2 (single unified hierarchy) and legacy v1 (separate per-controller
      hierarchies, as used by e.g. WSL2 in hybrid mode) -- see "Cgroups
      compatibility" below. Verified with a real OOM-kill: a 200MB
      allocation inside a 20MB-limited container gets killed rather than
      completing.
- [x] **Real OCI images** -- pulls and unpacks real images (e.g.
      `alpine:latest`) directly from Docker Hub's v2 Distribution API:
      anonymous token auth, manifest-list resolution for multi-arch
      images, and layer download/extraction. No `docker` CLI or daemon
      involved.

## How it works

### Namespaces + chroot

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

### Startup synchronization

The parent and child are synchronized via a pipe (`ExtraFiles` fd 3):
the child blocks immediately on entry until the parent signals that
cgroup setup is fully complete. Without this, a memory/CPU-heavy
command could run and finish *before* the parent finishes creating the
cgroup and adding the child's pid to it -- completely bypassing the
limit. This is not a hypothetical: an early version of this code had
exactly that race, and a 200MB allocation sailed straight through a
20MB limit because it simply finished before the cgroup was ready.

### Cgroups compatibility

Not every system delegates the `memory`/`cpu` controllers to the
cgroups v2 unified hierarchy at `/sys/fs/cgroup`. Notably, WSL2 mounts
a v2 tree but leaves `memory`/`cpu` attached to the legacy v1
per-controller hierarchies instead (a controller can only belong to one
hierarchy at a time). `internal/cgroups` detects which mode is active
(`cgroup.controllers` non-empty at the v2 root means v2 is usable) and
uses the matching API -- `memory.max`/`cpu.max` for v2, or
`memory.limit_in_bytes` + `cpu.cfs_quota_us`/`cpu.cfs_period_us` for v1.

It also caps `memory.memsw.limit_in_bytes` (memory **+ swap**) to the
same value as the memory limit. Without this, a process that hits its
memory limit can just get reclaimed to swap instead of OOM-killed --
it'll run (slowly) rather than actually respecting the limit. This was
found the hard way: a "killable" memory-hog test kept completing
successfully until swap was also capped.

### Image pulling

`myrun pull <image> <destdir>` (`internal/image`) talks directly to
Docker Hub's registry API:

1. Requests an anonymous pull token from `auth.docker.io` (no
   credentials needed for public images).
2. Fetches the manifest for the requested tag, resolving a manifest
   list (multi-architecture index) down to the `linux/amd64` manifest
   if the image is multi-arch.
3. Downloads each layer blob (gzipped tar) and streams it straight into
   an extractor -- no intermediate buffering of the whole layer in
   memory.

Known limitation: this does not implement OCI whiteout-file semantics
(a file named `.wh.<name>` in a layer means "delete `<name>` from
earlier layers"). This doesn't matter for simple, purely-additive
images like Alpine, but an image whose layers actually delete/replace
files from earlier ones would end up with stale files left behind.

## Usage

```sh
go build -o myrun ./cmd/myrun

# Pull a real image from Docker Hub
./myrun pull alpine:latest ./rootfs-alpine

# Run it, with resource limits
sudo ./myrun run --memory=50m --cpu=0.5 "$(pwd)/rootfs-alpine" /bin/sh
```

Inside the shell, try `hostname`, `ps`, `cat /etc/os-release`, and
`ls /` -- they'll show a real, isolated Alpine Linux environment.

Alternatively, for a minimal test rootfs that doesn't require registry
access:

```sh
./scripts/build-test-rootfs.sh
sudo ./myrun run "$(pwd)/rootfs-minimal" /bin/sh
```

### Verifying the memory limit is actually enforced

Don't take a clean run as proof the limit works -- verify it directly.
Compile the small test program below as a static binary into your
rootfs's `/bin`, then try to exceed the limit:

```c
// memhog.c -- allocates and touches N megabytes, to test memory limits
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
int main(int argc, char **argv) {
    int mb = argc > 1 ? atoi(argv[1]) : 200;
    char *buf = malloc((size_t)mb * 1024 * 1024);
    memset(buf, 1, (size_t)mb * 1024 * 1024); // force pages resident
    printf("done allocating %d MB\n", mb);
    return 0;
}
```

```sh
gcc -static -o rootfs-alpine/bin/memhog memhog.c
sudo ./myrun run --memory=20m "$(pwd)/rootfs-alpine" /bin/memhog 200
echo "exit code: $?"
# should be killed (signal: killed) before printing "done allocating",
# not complete successfully
```

## Known limitations

- The runtime process itself becomes PID 1 inside the namespace, and
  the user's command runs as its child (PID 2+), rather than the
  user's command being PID 1 directly. Real runtimes exec directly
  into the target command to avoid this. Candidate fix: use
  `syscall.Exec` instead of `exec.Command(...).Run()` in the child.
- No OCI whiteout-file support in the image extractor (see above)
- Container shares the host's network namespace -- no network
  isolation
- Requires root / appropriate capabilities to create namespaces
- Only tested against Docker Hub; other OCI-compatible registries
  (ghcr.io, etc.) may need auth-flow adjustments

## Requirements

- Linux (namespaces are Linux-specific; won't run on macOS/Windows
  natively -- this was developed and tested on WSL2/Ubuntu 24.04)
- Go 1.22+
- Root privileges (or `CAP_SYS_ADMIN`) to create namespaces
- Internet access to pull images (not required to use a hand-built
  rootfs)