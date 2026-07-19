// Package cgroups sets up resource limits for a container process, using
// whichever cgroup API this system actually delegates the memory/cpu
// controllers through.
//
// Most modern Linux distros run pure cgroups v2, with a single unified
// hierarchy at /sys/fs/cgroup. Some hybrid setups -- notably WSL2 -- mount
// a v2 tree but don't delegate memory/cpu to it; those controllers stay
// attached to the legacy v1 per-controller hierarchies
// (/sys/fs/cgroup/memory, /sys/fs/cgroup/cpu) instead, since a controller
// can only belong to one hierarchy at a time. This package detects which
// situation it's in and uses the matching API.
package cgroups

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

type cgroupMode int

const (
	modeV2 cgroupMode = iota
	modeV1
)

// detectMode checks whether the v2 unified hierarchy actually has
// controllers delegated to it. If not, falls back to v1.
func detectMode() (mode cgroupMode, v2Root string, err error) {
	for _, root := range []string{"/sys/fs/cgroup", "/sys/fs/cgroup/unified"} {
		data, readErr := os.ReadFile(filepath.Join(root, "cgroup.controllers"))
		if readErr == nil && strings.TrimSpace(string(data)) != "" {
			return modeV2, root, nil
		}
	}
	if _, statErr := os.Stat("/sys/fs/cgroup/memory"); statErr == nil {
		return modeV1, "", nil
	}
	return modeV2, "", fmt.Errorf("no usable cgroup hierarchy found (checked v2 unified and v1 legacy mounts)")
}

// Limits describes the resource caps to apply to a container. Zero-value
// fields mean "no limit set".
type Limits struct {
	MemoryBytes int64
	CPUQuota    float64 // fraction of one core, e.g. 0.5
}

// Group represents the cgroup(s) created for one container instance. In
// v2 mode this is a single cgroup covering both controllers; in v1 mode
// it's one cgroup per controller, since v1 splits them into separate
// hierarchies that each need the process's pid added independently.
type Group struct {
	mode  cgroupMode
	paths []string
}

// New creates cgroup(s) for a container and applies the given limits.
func New(name string, limits Limits) (*Group, error) {
	mode, v2Root, err := detectMode()
	if err != nil {
		return nil, err
	}

	g := &Group{mode: mode}

	if mode == modeV2 {
		path := filepath.Join(v2Root, "myrun", name)
		if err := os.MkdirAll(path, 0755); err != nil {
			return nil, fmt.Errorf("creating cgroup %s: %w", path, err)
		}
		if limits.MemoryBytes > 0 {
			if err := writeFile(path, "memory.max", strconv.FormatInt(limits.MemoryBytes, 10)); err != nil {
				return nil, fmt.Errorf("setting memory.max: %w", err)
			}
		}
		if limits.CPUQuota > 0 {
			const periodUs = 100000
			quotaUs := int64(limits.CPUQuota * periodUs)
			if err := writeFile(path, "cpu.max", fmt.Sprintf("%d %d", quotaUs, periodUs)); err != nil {
				return nil, fmt.Errorf("setting cpu.max: %w", err)
			}
		}
		g.paths = []string{path}
		return g, nil
	}

	// v1 mode: memory and cpu each get their own cgroup in their own
	// hierarchy.
	if limits.MemoryBytes > 0 {
		memPath := filepath.Join("/sys/fs/cgroup/memory/myrun", name)
		if err := os.MkdirAll(memPath, 0755); err != nil {
			return nil, fmt.Errorf("creating memory cgroup %s: %w", memPath, err)
		}
		if err := writeFile(memPath, "memory.limit_in_bytes", strconv.FormatInt(limits.MemoryBytes, 10)); err != nil {
    return nil, fmt.Errorf("setting memory.limit_in_bytes: %w", err)
}
// Also cap memory+swap combined to the same value. Without this,
// a process that hits memory.limit_in_bytes can just get its
// anonymous pages pushed to swap instead of OOM-killed, if swap
// is available on the host -- it'll run (slowly) rather than
// actually respecting the limit. Setting memsw equal to the
// memory limit removes that escape hatch. This may fail with
// ENOENT/ENOTSUP on kernels built without swap accounting, which
// is fine to ignore -- the plain memory limit is still applied.
_ = writeFile(memPath, "memory.memsw.limit_in_bytes", strconv.FormatInt(limits.MemoryBytes, 10))
		g.paths = append(g.paths, memPath)
	}

	if limits.CPUQuota > 0 {
		cpuPath := filepath.Join("/sys/fs/cgroup/cpu/myrun", name)
		if err := os.MkdirAll(cpuPath, 0755); err != nil {
			return nil, fmt.Errorf("creating cpu cgroup %s: %w", cpuPath, err)
		}
		const periodUs = 100000
		quotaUs := int64(limits.CPUQuota * periodUs)
		if err := writeFile(cpuPath, "cpu.cfs_period_us", strconv.FormatInt(periodUs, 10)); err != nil {
			return nil, fmt.Errorf("setting cpu.cfs_period_us: %w", err)
		}
		if err := writeFile(cpuPath, "cpu.cfs_quota_us", strconv.FormatInt(quotaUs, 10)); err != nil {
			return nil, fmt.Errorf("setting cpu.cfs_quota_us: %w", err)
		}
		g.paths = append(g.paths, cpuPath)
	}

	return g, nil
}

// AddProcess moves the given pid into every hierarchy this group manages.
// v2 uses "cgroup.procs"; v1 hierarchies use "tasks".
func (g *Group) AddProcess(pid int) error {
	procsFile := "cgroup.procs"
	if g.mode == modeV1 {
		procsFile = "tasks"
	}
	for _, path := range g.paths {
		if err := writeFile(path, procsFile, strconv.Itoa(pid)); err != nil {
			return fmt.Errorf("adding pid %d to %s: %w", pid, path, err)
		}
	}
	return nil
}

// Close removes all cgroups this group created. Only succeeds once no
// live processes remain in them (call after the container has exited).
func (g *Group) Close() error {
	var firstErr error
	for _, path := range g.paths {
		if err := os.Remove(path); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func writeFile(dir, file, value string) error {
	return os.WriteFile(filepath.Join(dir, file), []byte(value), 0644)
}