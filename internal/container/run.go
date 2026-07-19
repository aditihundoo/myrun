package container

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"time"

	"github.com/aditihundoo/myrun/internal/cgroups"
)

// Limits are the resource caps requested via CLI flags. Zero-value fields
// mean "no limit".
type Limits struct {
	MemoryBytes int64
	CPUQuota    float64
}

// Run is the parent-side entry point for `myrun run <rootfs> <cmd> <args...>`.
//
// It re-execs the myrun binary as `myrun child <rootfs> <cmd> <args...>`,
// but launches that re-exec inside brand new namespaces via Cloneflags.
// The child process (running Child(), below) is what actually sets up
// the chroot, mounts /proc, and finally execs the user's command.
//
// Startup is synchronized via a pipe: the child blocks on fd 3 immediately
// on entry and does not proceed until this function has finished creating
// the cgroup and adding the child's pid to it. Without this, a
// memory/CPU-heavy command could run and finish during the window between
// "child process started" and "child process joined its cgroup" --
// completely bypassing the limit. This bit us during development: an
// early version added the pid to the cgroup *after* starting the child,
// and a 200MB allocation sailed straight through a 20MB limit because it
// simply finished before the cgroup was ready.
func Run(rootfs, cmdName string, args []string, limits Limits) error {
	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolving self path: %w", err)
	}

	syncRead, syncWrite, err := os.Pipe()
	if err != nil {
		return fmt.Errorf("creating sync pipe: %w", err)
	}

	childArgs := append([]string{"child", rootfs, cmdName}, args...)
	cmd := exec.Command(self, childArgs...)

	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	// syncRead becomes fd 3 in the child (fds 0-2 are stdin/out/err).
	cmd.ExtraFiles = []*os.File{syncRead}

	cmd.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags: syscall.CLONE_NEWUTS | // hostname isolation
			syscall.CLONE_NEWPID | // process ID isolation (child becomes PID 1)
			syscall.CLONE_NEWNS | // mount namespace isolation
			syscall.CLONE_NEWIPC, // IPC isolation (no shared System V IPC/message queues)
	}

	if err := cmd.Start(); err != nil {
		syncRead.Close()
		syncWrite.Close()
		return fmt.Errorf("starting container process: %w", err)
	}
	// The parent doesn't need its copy of the read end; the child has its
	// own via ExtraFiles.
	syncRead.Close()

	groupName := fmt.Sprintf("container-%d-%d", cmd.Process.Pid, time.Now().UnixNano())
	group, cgErr := cgroups.New(groupName, cgroups.Limits{
		MemoryBytes: limits.MemoryBytes,
		CPUQuota:    limits.CPUQuota,
	})
	if cgErr != nil {
		fmt.Fprintf(os.Stderr, "myrun: warning: cgroup limits not applied: %v\n", cgErr)
		group = nil
	} else if err := group.AddProcess(cmd.Process.Pid); err != nil {
		fmt.Fprintf(os.Stderr, "myrun: warning: could not apply cgroup limits: %v\n", err)
	}

	// Signal the child it's safe to proceed -- cgroup setup (if any) is
	// done and the pid is already joined.
	if _, err := syncWrite.Write([]byte{1}); err != nil {
		fmt.Fprintf(os.Stderr, "myrun: warning: could not signal child readiness: %v\n", err)
	}
	syncWrite.Close()

	waitErr := cmd.Wait()

	if group != nil {
		if err := group.Close(); err != nil {
			fmt.Fprintf(os.Stderr, "myrun: warning: could not clean up cgroup: %v\n", err)
		}
	}

	if waitErr != nil {
		return fmt.Errorf("running container: %w", waitErr)
	}
	return nil
}