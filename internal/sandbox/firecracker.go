package sandbox

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	kilnv1 "github.com/tmjoris/kiln/gen/kiln/v1"
	"github.com/tmjoris/kiln/internal/firecracker"
	"github.com/tmjoris/kiln/internal/guest"
)

type FirecrackerConfig struct {
	Binary    string
	Kernel    string
	ImagesDir string // <ImagesDir>/<template>/rootfs.ext4
	DataDir   string
	BootArgs  string
	Default   Spec
	Warmup    []string // argv run in the template before snapshotting
}

type Firecracker struct {
	cfg   FirecrackerConfig
	mu    sync.Mutex
	snaps map[Spec]*snapshot
}

type snapshot struct {
	once       sync.Once
	err        error
	state, mem string
}

func NewFirecracker(cfg FirecrackerConfig) (*Firecracker, error) {
	// Firecracker runs with each VM's directory as cwd, so every path we
	// hand a backend must be absolute.
	for _, p := range []*string{&cfg.Binary, &cfg.Kernel, &cfg.ImagesDir, &cfg.DataDir} {
		abs, err := filepath.Abs(*p)
		if err != nil {
			return nil, err
		}
		*p = abs
	}
	for _, p := range []string{cfg.Binary, cfg.Kernel, "/dev/kvm"} {
		if _, err := os.Stat(p); err != nil {
			return nil, fmt.Errorf("firecracker backend: %w", err)
		}
	}
	// Snapshots embed guest state from this kernel/rootfs/firecracker build,
	// so they are rebuilt every start instead of cached across upgrades.
	os.RemoveAll(filepath.Join(cfg.DataDir, "snapshots"))
	return &Firecracker{cfg: cfg, snaps: map[Spec]*snapshot{}}, nil
}

func (f *Firecracker) Kind() kilnv1.Backend { return kilnv1.Backend_BACKEND_FIRECRACKER }

func (f *Firecracker) Resolve(s Spec) (Spec, error) {
	if s.Template == "" {
		s.Template = f.cfg.Default.Template
	}
	if s.VCPUs == 0 {
		s.VCPUs = f.cfg.Default.VCPUs
	}
	if s.MemMiB == 0 {
		s.MemMiB = f.cfg.Default.MemMiB
	}
	if _, err := os.Stat(f.rootfs(s)); err != nil {
		return s, fmt.Errorf("unknown template %q", s.Template)
	}
	if s.VCPUs > 32 || s.MemMiB < 128 {
		return s, fmt.Errorf("unsupported shape %dx%d", s.VCPUs, s.MemMiB)
	}
	return s, nil
}

func (f *Firecracker) rootfs(s Spec) string {
	return filepath.Join(f.cfg.ImagesDir, s.Template, "rootfs.ext4")
}

func (f *Firecracker) boot(id, dir string, s Spec) (*firecracker.VM, error) {
	return firecracker.Boot(firecracker.Binary{Firecracker: f.cfg.Binary}, firecracker.BootSpec{
		ID: id, Dir: dir, Kernel: f.cfg.Kernel, Rootfs: f.rootfs(s),
		BootArgs: f.cfg.BootArgs, VCPUs: s.VCPUs, MemMiB: s.MemMiB,
	})
}

// Prepare boots a template VM, waits for its agent, and snapshots it. Every
// later sandbox of this shape is a copy-on-write clone of that moment.
func (f *Firecracker) Prepare(ctx context.Context, s Spec) error {
	_, err := f.prepare(ctx, s)
	return err
}

func (f *Firecracker) prepare(ctx context.Context, s Spec) (*snapshot, error) {
	f.mu.Lock()
	sn, ok := f.snaps[s]
	if !ok {
		sn = &snapshot{}
		f.snaps[s] = sn
	}
	f.mu.Unlock()
	sn.once.Do(func() {
		dir := filepath.Join(f.cfg.DataDir, "snapshots", fmt.Sprintf("%s-%dx%d", s.Template, s.VCPUs, s.MemMiB))
		sn.state, sn.mem = filepath.Join(dir, "vm.snap"), filepath.Join(dir, "mem.snap")
		os.RemoveAll(dir)
		sn.err = f.buildSnapshot(ctx, dir, s, sn.state, sn.mem)
	})
	if sn.err != nil {
		// Do not cache failures: the next caller tries again.
		f.mu.Lock()
		if f.snaps[s] == sn {
			delete(f.snaps, s)
		}
		f.mu.Unlock()
		return nil, sn.err
	}
	return sn, nil
}

func (f *Firecracker) buildSnapshot(ctx context.Context, dir string, s Spec, state, mem string) error {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	vm, err := f.boot("template", filepath.Join(dir, "vm"), s)
	if err != nil {
		return err
	}
	defer vm.Stop()
	c, err := guest.Connect(ctx, vm.DialAgent)
	if err != nil {
		return fmt.Errorf("template boot: %w", err)
	}
	// Run the warmup with the template so every clone starts with the
	// interpreter and stdlib already in guest page cache.
	if len(f.cfg.Warmup) > 0 {
		if err := drainExec(ctx, c, f.cfg.Warmup); err != nil {
			c.Close()
			return fmt.Errorf("template warmup: %w", err)
		}
	}
	c.Close()
	return vm.Snapshot(ctx, state, mem)
}

func (f *Firecracker) Start(ctx context.Context, id string, s Spec, cold bool) (Instance, kilnv1.StartMode, error) {
	dir := filepath.Join(f.cfg.DataDir, "vms", id)
	var (
		vm   *firecracker.VM
		err  error
		mode = kilnv1.StartMode_START_MODE_COLD_BOOT
	)
	if !cold {
		var sn *snapshot
		if sn, err = f.prepare(ctx, s); err != nil {
			return nil, 0, fmt.Errorf("snapshot: %w", err)
		}
		mode = kilnv1.StartMode_START_MODE_SNAPSHOT_RESTORE
		vm, err = firecracker.Restore(ctx, firecracker.Binary{Firecracker: f.cfg.Binary}, firecracker.RestoreSpec{
			ID: id, Dir: dir, Rootfs: f.rootfs(s), StatePath: sn.state, MemPath: sn.mem,
		})
	} else {
		vm, err = f.boot(id, dir, s)
	}
	if err != nil {
		os.RemoveAll(dir)
		return nil, 0, err
	}
	c, err := guest.Connect(ctx, vm.DialAgent)
	if err != nil {
		vm.Stop()
		return nil, 0, err
	}
	return &fcInstance{vm: vm, c: c}, mode, nil
}

type fcInstance struct {
	vm *firecracker.VM
	c  *guest.Client
}

func (i *fcInstance) Guest() *guest.Client    { return i.c }
func (i *fcInstance) Pid() int                { return i.vm.Pid() }
func (i *fcInstance) Exited() <-chan struct{} { return i.vm.Exited() }
func (i *fcInstance) Destroy() error {
	i.c.Close()
	return i.vm.Stop()
}
