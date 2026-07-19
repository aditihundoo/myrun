package container

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"
)

// Child runs inside the namespaces set up by Run(). It:
//  0. waits for a "ready" signal from the parent on fd 3 -- this is
//     critical, not optional: without it, this process could allocate
//     memory or spawn work *before* the parent has finished creating the
//     cgroup and adding this pid to it, which would let a
//     memory/CPU-heavy command slip past its supposed limit entirely
//     during that startup window. Real runtimes (runc etc.) use the same
//     pattern for the same reason.
//  1. sets a container-local hostname (isolated by CLONE_NEWUTS)
//  2. chroots into rootfs so the container can't see the host filesystem
//  3. mounts a fresh /proc so tools like `ps` only see container processes
//     (this only shows container-only processes because we're inside a new
//     PID namespace -- mounting /proc alone doesn't isolate anything, the
//     namespace does the isolating, the mount just gives you a view of it)
//  4. execs the user's requested command as PID 1 of the container
func Child(rootfs, cmdName string, args []string) error {
	if err := waitForReady(); err != nil {
		return fmt.Errorf("waiting for parent readiness signal: %w", err)
	}

	if err := syscall.Sethostname([]byte("myrun-container")); err != nil {
		return fmt.Errorf("sethostname: %w", err)
	}

	if err := syscall.Chroot(rootfs); err != nil {
		return fmt.Errorf("chroot into %s: %w", rootfs, err)
	}
	if err := os.Chdir("/"); err != nil {
		return fmt.Errorf("chdir to new root: %w", err)
	}

	// Mount a fresh procfs inside the new root. Without this, `ps` etc.
	// inside the container would either fail or show the host's /proc
	// (if it somehow leaked through), because chroot alone does not
	// remount filesystems -- it only changes the apparent root directory.
	if err := os.MkdirAll("/proc", 0755); err != nil {
		return fmt.Errorf("mkdir /proc: %w", err)
	}
	if err := syscall.Mount("proc", "/proc", "proc", 0, ""); err != nil {
		return fmt.Errorf("mount /proc: %w", err)
	}
	defer syscall.Unmount("/proc", 0)

	cmd := exec.Command(cmdName, args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("running %s inside container: %w", cmdName, err)
	}
	return nil
}

// waitForReady blocks until the parent writes a byte on fd 3 (passed via
// cmd.ExtraFiles), signaling that cgroup setup is complete and this
// process has already been added to its resource-limited cgroup.
func waitForReady() error {
	syncFile := os.NewFile(3, "sync-pipe")
	if syncFile == nil {
		return fmt.Errorf("sync pipe (fd 3) not available")
	}
	defer syncFile.Close()

	buf := make([]byte, 1)
	if _, err := syncFile.Read(buf); err != nil {
		return fmt.Errorf("reading readiness signal: %w", err)
	}
	return nil
}