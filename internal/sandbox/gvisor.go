package sandbox

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sync"

	kilnv1 "github.com/tmjoris/kiln/gen/kiln/v1"
	"github.com/tmjoris/kiln/internal/guest"
	"github.com/tmjoris/kiln/internal/vsock"
)

type GVisorConfig struct {
	Runsc     string
	ImagesDir string // <ImagesDir>/<template>/rootfs.tar
	DataDir   string
	Platform  string // systrap (default) or kvm
	// IgnoreCgroups skips cgroup setup; needed when the host's cgroup tree
	// is not delegated to us (e.g. inside a dev container).
	IgnoreCgroups bool
	Default       Spec
}

// GVisor runs each sandbox as a runsc container. The rootfs directory is
// shared read-only between sandboxes; runsc's memory-backed overlay absorbs
// writes, mirroring the Firecracker design.
type GVisor struct {
	cfg     GVisorConfig
	mu      sync.Mutex
	rootfss map[string]*extracted
}

type extracted struct {
	once sync.Once
	dir  string
	err  error
}

func NewGVisor(cfg GVisorConfig) (*GVisor, error) {
	// Firecracker runs with each VM's directory as cwd, so every path we
	// hand a backend must be absolute.
	for _, p := range []*string{&cfg.Runsc, &cfg.ImagesDir, &cfg.DataDir} {
		abs, err := filepath.Abs(*p)
		if err != nil {
			return nil, err
		}
		*p = abs
	}
	if _, err := os.Stat(cfg.Runsc); err != nil {
		return nil, fmt.Errorf("gvisor backend: %w", err)
	}
	if cfg.Platform == "" {
		cfg.Platform = "systrap"
	}
	return &GVisor{cfg: cfg, rootfss: map[string]*extracted{}}, nil
}

func (g *GVisor) Kind() kilnv1.Backend { return kilnv1.Backend_BACKEND_GVISOR }

func (g *GVisor) Resolve(s Spec) (Spec, error) {
	if s.Template == "" {
		s.Template = g.cfg.Default.Template
	}
	if s.VCPUs == 0 {
		s.VCPUs = g.cfg.Default.VCPUs
	}
	if s.MemMiB == 0 {
		s.MemMiB = g.cfg.Default.MemMiB
	}
	if _, err := os.Stat(filepath.Join(g.cfg.ImagesDir, s.Template, "rootfs.tar")); err != nil {
		return s, fmt.Errorf("unknown template %q", s.Template)
	}
	return s, nil
}

// Prepare unpacks the template rootfs once per daemon start.
func (g *GVisor) Prepare(ctx context.Context, s Spec) error {
	g.mu.Lock()
	e, ok := g.rootfss[s.Template]
	if !ok {
		e = &extracted{dir: filepath.Join(g.cfg.DataDir, "gvisor", "rootfs", s.Template)}
		g.rootfss[s.Template] = e
	}
	g.mu.Unlock()
	e.once.Do(func() {
		os.RemoveAll(e.dir)
		if e.err = os.MkdirAll(e.dir, 0o755); e.err != nil {
			return
		}
		tar := filepath.Join(g.cfg.ImagesDir, s.Template, "rootfs.tar")
		out, err := exec.CommandContext(ctx, "tar", "--numeric-owner", "-xf", tar, "-C", e.dir).CombinedOutput()
		if err != nil {
			e.err = fmt.Errorf("extract %s: %v: %s", tar, err, out)
		}
	})
	if e.err != nil {
		g.mu.Lock()
		if g.rootfss[s.Template] == e {
			delete(g.rootfss, s.Template)
		}
		g.mu.Unlock()
	}
	return e.err
}

func (g *GVisor) runsc(ctx context.Context, args ...string) ([]byte, error) {
	base := []string{"--root", filepath.Join(g.cfg.DataDir, "gvisor", "state")}
	cmd := exec.CommandContext(ctx, g.cfg.Runsc, append(base, args...)...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return out, fmt.Errorf("runsc %s: %v: %s", args[0], err, bytes.TrimSpace(stderr.Bytes()))
	}
	return out, nil
}

func (g *GVisor) Start(ctx context.Context, id string, s Spec, _ bool) (Instance, kilnv1.StartMode, error) {
	if err := g.Prepare(ctx, s); err != nil {
		return nil, 0, err
	}
	bundle := filepath.Join(g.cfg.DataDir, "gvisor", "bundles", id)
	sockDir := filepath.Join(bundle, "sock")
	if err := os.MkdirAll(sockDir, 0o750); err != nil {
		return nil, 0, err
	}
	g.mu.Lock()
	rootfs := g.rootfss[s.Template].dir
	g.mu.Unlock()
	spec := ociSpec(id, rootfs, sockDir, s)
	b, _ := json.Marshal(spec)
	if err := os.WriteFile(filepath.Join(bundle, "config.json"), b, 0o640); err != nil {
		return nil, 0, err
	}
	args := []string{
		"--network=none", "--host-uds=create", "--overlay2=root:memory",
		"--platform=" + g.cfg.Platform,
	}
	if g.cfg.IgnoreCgroups {
		args = append(args, "--ignore-cgroups")
	}
	args = append(args, "run", "--detach", "--bundle", bundle, id)
	if _, err := g.runsc(ctx, args...); err != nil {
		os.RemoveAll(bundle)
		return nil, 0, err
	}
	inst := &gvInstance{g: g, id: id, bundle: bundle}
	sock := filepath.Join(sockDir, "agent.sock")
	c, err := guest.Connect(ctx, func(ctx context.Context) (net.Conn, error) {
		return vsock.DialUnix(ctx, sock)
	})
	if err != nil {
		inst.Destroy()
		return nil, 0, err
	}
	inst.c = c
	return inst, kilnv1.StartMode_START_MODE_COLD_BOOT, nil
}

type gvInstance struct {
	g      *GVisor
	id     string
	bundle string
	c      *guest.Client
}

func (i *gvInstance) Guest() *guest.Client    { return i.c }
func (i *gvInstance) Exited() <-chan struct{} { return nil }

func (i *gvInstance) Pid() int {
	out, err := i.g.runsc(context.Background(), "state", i.id)
	if err != nil {
		return 0
	}
	var st struct {
		Pid int `json:"pid"`
	}
	json.Unmarshal(out, &st)
	return st.Pid
}

func (i *gvInstance) Destroy() error {
	if i.c != nil {
		i.c.Close()
	}
	ctx := context.Background()
	i.g.runsc(ctx, "kill", i.id, "KILL")
	_, err := i.g.runsc(ctx, "delete", "--force", i.id)
	os.RemoveAll(i.bundle)
	return err
}

// ociSpec is the minimal OCI runtime spec for a sandbox: agent as init,
// shared rootfs, a private socket directory, and resource limits.
func ociSpec(id, rootfs, sockDir string, s Spec) map[string]any {
	caps := []string{
		"CAP_CHOWN", "CAP_DAC_OVERRIDE", "CAP_FSETID", "CAP_FOWNER", "CAP_MKNOD", "CAP_SETGID",
		"CAP_SETUID", "CAP_SETFCAP", "CAP_SETPCAP", "CAP_NET_BIND_SERVICE", "CAP_SYS_CHROOT",
		"CAP_KILL", "CAP_AUDIT_WRITE",
	}
	period := uint64(100000)
	return map[string]any{
		"ociVersion": "1.0.0",
		"hostname":   id,
		"process": map[string]any{
			"terminal": false,
			"user":     map[string]any{"uid": 0, "gid": 0},
			"args":     []string{"/kiln/agent", "-listen", "unix:/run/kiln/agent.sock"},
			"env":      []string{"PATH=/usr/local/bin:/usr/bin:/bin", "KILN_BACKEND=gvisor"},
			"cwd":      "/",
			"capabilities": map[string]any{
				"bounding": caps, "effective": caps, "permitted": caps,
			},
			"noNewPrivileges": true,
		},
		"root": map[string]any{"path": rootfs, "readonly": false},
		"mounts": []map[string]any{
			{"destination": "/proc", "type": "proc", "source": "proc"},
			{"destination": "/dev", "type": "tmpfs", "source": "tmpfs", "options": []string{"nosuid", "strictatime", "mode=755", "size=65536k"}},
			{"destination": "/dev/pts", "type": "devpts", "source": "devpts", "options": []string{"nosuid", "noexec", "newinstance", "ptmxmode=0666", "mode=0620"}},
			{"destination": "/dev/shm", "type": "tmpfs", "source": "shm", "options": []string{"nosuid", "noexec", "nodev", "mode=1777", "size=65536k"}},
			{"destination": "/sys", "type": "sysfs", "source": "sysfs", "options": []string{"nosuid", "noexec", "nodev", "ro"}},
			{"destination": "/run/kiln", "type": "bind", "source": sockDir, "options": []string{"rbind", "rw"}},
		},
		"linux": map[string]any{
			"namespaces": []map[string]string{
				{"type": "pid"}, {"type": "network"}, {"type": "ipc"}, {"type": "uts"}, {"type": "mount"},
			},
			"resources": map[string]any{
				"memory": map[string]any{"limit": int64(s.MemMiB) << 20},
				"cpu":    map[string]any{"quota": int64(period) * int64(s.VCPUs), "period": period},
			},
		},
	}
}
