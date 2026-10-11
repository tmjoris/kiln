package firecracker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/tmjoris/kiln/internal/vsock"
)

// Every VM runs with its own directory as the working directory, and all
// paths baked into a snapshot are relative to it. That is what lets many VMs
// restore from one snapshot without fighting over the vsock socket path or
// the root drive (the same trick the jailer's chroot provides).
const (
	rootfsName = "rootfs.ext4"
	vsockName  = "v.sock"
	apiName    = "api.sock"
	guestCID   = 3
)

// DefaultBootArgs keeps boot lean: no PCI probing, no i8042 probing, quiet
// console, the agent as init, and a read-only root (the agent overlays it).
const DefaultBootArgs = "console=ttyS0 reboot=k panic=1 pci=off nomodule quiet loglevel=1 " +
	"i8042.noaux i8042.nomux i8042.nopnp i8042.dumbkbd random.trust_cpu=on " +
	"ro init=/kiln/agent"

type Binary struct {
	Firecracker string // path to firecracker binary
}

// VM is a running Firecracker process plus its working directory.
type VM struct {
	ID  string
	Dir string
	cmd *exec.Cmd
	api *apiClient
	// exited is closed when the firecracker process exits.
	exited chan struct{}
	err    error
}

func (vm *VM) Pid() int          { return vm.cmd.Process.Pid }
func (vm *VM) VsockPath() string { return filepath.Join(vm.Dir, vsockName) }

// DialAgent opens a stream to the in-guest agent.
func (vm *VM) DialAgent(ctx context.Context) (net.Conn, error) {
	select {
	case <-vm.exited:
		return nil, fmt.Errorf("vm %s exited: %v (see %s)", vm.ID, vm.err, filepath.Join(vm.Dir, "firecracker.log"))
	default:
	}
	return vsock.DialFirecracker(ctx, vm.VsockPath(), vsock.AgentPort)
}

func (vm *VM) Exited() <-chan struct{} { return vm.exited }

// Stop kills the VM and removes its directory. Firecracker holds no state we
// need to flush: the root drive is read-only and writes live in guest RAM.
func (vm *VM) Stop() error {
	vm.cmd.Process.Signal(syscall.SIGKILL)
	<-vm.exited
	return os.RemoveAll(vm.Dir)
}

// Pause and Snapshot write a full snapshot (vmstate + guest memory).
func (vm *VM) Snapshot(ctx context.Context, statePath, memPath string) error {
	if err := vm.api.do(ctx, "PATCH", "/vm", map[string]string{"state": "Paused"}); err != nil {
		return err
	}
	return vm.api.do(ctx, "PUT", "/snapshot/create", snapshotCreate{
		SnapshotType: "Full", SnapshotPath: statePath, MemFilePath: memPath,
	})
}

func prepareDir(dir, rootfs string) error {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}
	abs, err := filepath.Abs(rootfs)
	if err != nil {
		return err
	}
	return os.Symlink(abs, filepath.Join(dir, rootfsName))
}

func spawn(bin Binary, id, dir string, extraArgs ...string) (*VM, error) {
	logf, err := os.Create(filepath.Join(dir, "firecracker.log"))
	if err != nil {
		return nil, err
	}
	defer logf.Close()
	args := append([]string{"--api-sock", apiName, "--id", id, "--level", "Warning"}, extraArgs...)
	cmd := exec.Command(bin.Firecracker, args...)
	cmd.Dir = dir
	cmd.Stdin = nil
	cmd.Stdout = logf
	cmd.Stderr = logf
	// Never outlive kilnd; keep VMs out of kilnd's signal group.
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL, Setpgid: true}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	vm := &VM{ID: id, Dir: dir, cmd: cmd, api: newAPIClient(filepath.Join(dir, apiName)), exited: make(chan struct{})}
	go func() {
		vm.err = cmd.Wait()
		close(vm.exited)
	}()
	return vm, nil
}

type BootSpec struct {
	ID       string
	Dir      string // created; must not exist
	Kernel   string
	Rootfs   string
	BootArgs string
	VCPUs    uint32
	MemMiB   uint32
}

// Boot starts a VM from kernel + rootfs. All configuration goes in through
// --config-file so the VM starts without any API round trips.
func Boot(bin Binary, s BootSpec) (*VM, error) {
	if err := prepareDir(s.Dir, s.Rootfs); err != nil {
		return nil, err
	}
	kernel, err := filepath.Abs(s.Kernel)
	if err != nil {
		return nil, err
	}
	args := s.BootArgs
	if args == "" {
		args = DefaultBootArgs
	}
	cfg := Config{
		BootSource:    BootSource{KernelImagePath: kernel, BootArgs: args},
		Drives:        []Drive{{DriveID: "rootfs", PathOnHost: rootfsName, IsRootDevice: true, IsReadOnly: true}},
		MachineConfig: MachineConfig{VcpuCount: s.VCPUs, MemSizeMib: s.MemMiB},
		Vsock:         &Vsock{GuestCID: guestCID, UDSPath: vsockName},
		Entropy:       &struct{}{},
	}
	b, err := json.Marshal(cfg)
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(s.Dir, "config.json"), b, 0o640); err != nil {
		return nil, err
	}
	return spawn(bin, s.ID, s.Dir, "--config-file", "config.json")
}

type RestoreSpec struct {
	ID        string
	Dir       string
	Rootfs    string
	StatePath string
	MemPath   string
}

// Restore starts a VM from a snapshot. Guest memory is mapped MAP_PRIVATE
// from the snapshot file: pages are faulted in lazily and every VM restored
// from the same file shares the clean ones through the page cache.
func Restore(ctx context.Context, bin Binary, s RestoreSpec) (*VM, error) {
	if err := prepareDir(s.Dir, s.Rootfs); err != nil {
		return nil, err
	}
	vm, err := spawn(bin, s.ID, s.Dir)
	if err != nil {
		return nil, err
	}
	if err := waitForFile(ctx, filepath.Join(s.Dir, apiName), vm.exited); err != nil {
		vm.Stop()
		return nil, err
	}
	err = vm.api.do(ctx, "PUT", "/snapshot/load", snapshotLoad{
		SnapshotPath: s.StatePath,
		MemBackend:   memBackend{BackendType: "File", BackendPath: s.MemPath},
		ResumeVM:     true,
	})
	if err != nil {
		vm.Stop()
		return nil, err
	}
	return vm, nil
}

func waitForFile(ctx context.Context, path string, exited <-chan struct{}) error {
	for {
		if _, err := os.Stat(path); err == nil {
			return nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-exited:
			return fmt.Errorf("firecracker exited before creating %s", filepath.Base(path))
		case <-time.After(500 * time.Microsecond):
		}
	}
}

// ProcessMemory returns the Firecracker process's RSS and PSS in bytes. PSS
// divides shared pages among their sharers, so summing PSS over all VMs gives
// the true host cost of N snapshot clones.
func ProcessMemory(pid int) (rss, pss uint64, err error) {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/smaps_rollup", pid))
	if err != nil {
		return 0, 0, err
	}
	for _, line := range strings.Split(string(b), "\n") {
		var kb uint64
		switch {
		case strings.HasPrefix(line, "Rss:"):
			fmt.Sscanf(strings.TrimPrefix(line, "Rss:"), "%d", &kb)
			rss = kb << 10
		case strings.HasPrefix(line, "Pss:"):
			fmt.Sscanf(strings.TrimPrefix(line, "Pss:"), "%d", &kb)
			pss = kb << 10
		}
	}
	return rss, pss, nil
}
