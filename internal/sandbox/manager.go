package sandbox

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"

	agentv1 "github.com/tmjoris/kiln/gen/kiln/agent/v1"
	kilnv1 "github.com/tmjoris/kiln/gen/kiln/v1"
	"github.com/tmjoris/kiln/internal/guest"
)

type Config struct {
	Backends       []Backend
	DefaultBackend kilnv1.Backend
	// Pools maps "backend/template" to a target warm-pool size for the
	// template's default shape, e.g. {"firecracker/python": 8}.
	Pools       map[string]int
	IdleTimeout time.Duration
	// Admission control: refuse new guests past MaxSandboxes or when host
	// MemAvailable would drop below MinMemAvailable.
	MaxSandboxes    int
	MinMemAvailable uint64
	// MaxConcurrentStarts bounds pool refills (on-demand creates bypass it).
	MaxConcurrentStarts int
	// PoolWarmup runs in each pooled guest before it is parked, so the
	// first-touch cost (page faults into snapshot memory, cold file caches)
	// is paid off the request path.
	PoolWarmup []string
}

type Sandbox struct {
	ID        string
	Backend   kilnv1.Backend
	Spec      Spec
	Created   time.Time
	Startup   *kilnv1.StartupInfo
	Labels    map[string]string
	inst      Instance
	idle      time.Duration
	lastUsed  atomic.Int64
	closeOnce sync.Once
}

func (s *Sandbox) Guest() *guest.Client { return s.inst.Guest() }
func (s *Sandbox) Pid() int             { return s.inst.Pid() }
func (s *Sandbox) touch()               { s.lastUsed.Store(time.Now().UnixNano()) }

type Manager struct {
	cfg      Config
	backends map[kilnv1.Backend]Backend
	pools    map[string]*pool
	startSem chan struct{}
	live     atomic.Int32 // sandboxes handed out + pooled + starting

	mu        sync.RWMutex
	sandboxes map[string]*Sandbox

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

func poolKey(b kilnv1.Backend, s Spec) string { return backendName(b) + "/" + s.String() }

func backendName(b kilnv1.Backend) string {
	return strings.ToLower(strings.TrimPrefix(b.String(), "BACKEND_"))
}

func ParseBackend(name string) (kilnv1.Backend, error) {
	v, ok := kilnv1.Backend_value["BACKEND_"+strings.ToUpper(name)]
	if !ok || v == 0 {
		return 0, fmt.Errorf("unknown backend %q", name)
	}
	return kilnv1.Backend(v), nil
}

func NewManager(cfg Config) (*Manager, error) {
	if cfg.MaxConcurrentStarts <= 0 {
		cfg.MaxConcurrentStarts = runtime.NumCPU()
	}
	if cfg.IdleTimeout <= 0 {
		cfg.IdleTimeout = 30 * time.Minute
	}
	ctx, cancel := context.WithCancel(context.Background())
	m := &Manager{
		cfg:       cfg,
		backends:  map[kilnv1.Backend]Backend{},
		pools:     map[string]*pool{},
		startSem:  make(chan struct{}, cfg.MaxConcurrentStarts),
		sandboxes: map[string]*Sandbox{},
		ctx:       ctx,
		cancel:    cancel,
	}
	for _, b := range cfg.Backends {
		m.backends[b.Kind()] = b
	}
	if _, ok := m.backends[cfg.DefaultBackend]; !ok {
		return nil, fmt.Errorf("default backend %s not configured", backendName(cfg.DefaultBackend))
	}
	for name, target := range cfg.Pools {
		bname, tmpl, _ := strings.Cut(name, "/")
		bk, err := ParseBackend(bname)
		if err != nil {
			return nil, err
		}
		b, ok := m.backends[bk]
		if !ok {
			return nil, fmt.Errorf("pool %q: backend not configured", name)
		}
		spec, err := b.Resolve(Spec{Template: tmpl})
		if err != nil {
			return nil, fmt.Errorf("pool %q: %w", name, err)
		}
		p := newPool(m, b, spec, target)
		m.pools[p.key()] = p
	}
	for _, p := range m.pools {
		m.wg.Add(1)
		go func() { defer m.wg.Done(); p.run(ctx) }()
	}
	m.wg.Add(1)
	go func() { defer m.wg.Done(); m.reapIdle(ctx) }()
	return m, nil
}

// admit reserves capacity for one more guest. Callers that get true must
// eventually call release (Destroy does).
func (m *Manager) admit() bool {
	if m.cfg.MaxSandboxes > 0 && int(m.live.Load()) >= m.cfg.MaxSandboxes {
		return false
	}
	if m.cfg.MinMemAvailable > 0 {
		if avail, _, err := HostMemory(); err == nil && avail < m.cfg.MinMemAvailable {
			return false
		}
	}
	m.live.Add(1)
	return true
}

func (m *Manager) release() { m.live.Add(-1) }

type CreateOptions struct {
	Backend     kilnv1.Backend
	Spec        Spec
	IdleTimeout time.Duration
	Labels      map[string]string
	ForceCold   bool
	SkipPool    bool
}

func (m *Manager) Create(ctx context.Context, o CreateOptions) (*Sandbox, error) {
	t0 := time.Now()
	if o.Backend == kilnv1.Backend_BACKEND_UNSPECIFIED {
		o.Backend = m.cfg.DefaultBackend
	}
	b, ok := m.backends[o.Backend]
	if !ok {
		return nil, status.Errorf(codes.InvalidArgument, "backend %s not enabled", backendName(o.Backend))
	}
	spec, err := b.Resolve(o.Spec)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}

	var g pooled
	hit := false
	if !o.ForceCold && !o.SkipPool {
		if p, ok := m.pools[poolKey(o.Backend, spec)]; ok {
			g, hit = p.get()
		}
	}
	if hit {
		g.mode = kilnv1.StartMode_START_MODE_WARM_POOL
	} else {
		if !m.admit() {
			return nil, status.Error(codes.ResourceExhausted, "host at capacity")
		}
		g.id = newID()
		g.inst, g.mode, err = b.Start(ctx, g.id, spec, o.ForceCold)
		if err != nil {
			m.release()
			return nil, status.Errorf(codes.Unavailable, "start sandbox: %v", err)
		}
	}

	// Personalise: clones share the template's clock, RNG and hostname.
	entropy := make([]byte, 64)
	rand.Read(entropy)
	if _, err := g.inst.Guest().Init(ctx, &agentv1.InitRequest{
		UnixNanos: time.Now().UnixNano(), Entropy: entropy, Hostname: g.id,
	}); err != nil {
		g.inst.Destroy()
		m.release()
		return nil, status.Errorf(codes.Unavailable, "init sandbox: %v", err)
	}

	idle := o.IdleTimeout
	if idle <= 0 {
		idle = m.cfg.IdleTimeout
	}
	sb := &Sandbox{
		ID: g.id, Backend: o.Backend, Spec: spec, Created: time.Now(), Labels: o.Labels,
		inst: g.inst, idle: idle,
		Startup: &kilnv1.StartupInfo{Mode: g.mode},
	}
	sb.Startup.ReadyLatency = durationpb.New(time.Since(t0))
	sb.touch()
	m.mu.Lock()
	m.sandboxes[sb.ID] = sb
	m.mu.Unlock()
	if ex := g.inst.Exited(); ex != nil {
		go func() {
			select {
			case <-ex:
				// Delete removes the sandbox from the map before stopping
				// it, so finding it here means nobody asked it to exit.
				m.mu.RLock()
				_, live := m.sandboxes[sb.ID]
				m.mu.RUnlock()
				if live {
					slog.Warn("sandbox exited unexpectedly", "id", sb.ID)
					m.Delete(sb.ID)
				}
			case <-m.ctx.Done():
			}
		}()
	}
	observeStart(o.Backend, g.mode, time.Since(t0))
	return sb, nil
}

var ErrNotFound = errors.New("sandbox not found")

func (m *Manager) Get(id string) (*Sandbox, error) {
	m.mu.RLock()
	sb, ok := m.sandboxes[id]
	m.mu.RUnlock()
	if !ok {
		return nil, status.Errorf(codes.NotFound, "sandbox %q not found", id)
	}
	sb.touch()
	return sb, nil
}

func (m *Manager) List() []*Sandbox {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*Sandbox, 0, len(m.sandboxes))
	for _, sb := range m.sandboxes {
		out = append(out, sb)
	}
	return out
}

func (m *Manager) Delete(id string) error {
	m.mu.Lock()
	sb, ok := m.sandboxes[id]
	delete(m.sandboxes, id)
	m.mu.Unlock()
	if !ok {
		return status.Errorf(codes.NotFound, "sandbox %q not found", id)
	}
	var err error
	sb.closeOnce.Do(func() {
		err = sb.inst.Destroy()
		m.release()
	})
	return err
}

func (m *Manager) reapIdle(ctx context.Context) {
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		now := time.Now().UnixNano()
		for _, sb := range m.List() {
			if time.Duration(now-sb.lastUsed.Load()) > sb.idle {
				slog.Info("reaping idle sandbox", "id", sb.ID)
				m.Delete(sb.ID)
			}
		}
	}
}

type PoolStat struct {
	Key                     string
	Ready, Target, Starting int
}

func (m *Manager) Stats() (active int, pools []PoolStat) {
	m.mu.RLock()
	active = len(m.sandboxes)
	m.mu.RUnlock()
	for k, p := range m.pools {
		pools = append(pools, PoolStat{Key: k, Ready: len(p.ready), Target: p.target, Starting: int(p.starting.Load())})
	}
	return active, pools
}

// Close destroys every sandbox and pooled guest.
func (m *Manager) Close() {
	m.cancel()
	m.wg.Wait()
	var wg sync.WaitGroup
	for _, sb := range m.List() {
		wg.Add(1)
		go func() { defer wg.Done(); m.Delete(sb.ID) }()
	}
	wg.Wait()
}

func newID() string {
	b := make([]byte, 6)
	rand.Read(b)
	return "sb-" + hex.EncodeToString(b)
}

// HostMemory reads MemAvailable and MemTotal from /proc/meminfo, in bytes.
func HostMemory() (avail, total uint64, err error) {
	b, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0, 0, err
	}
	for _, line := range strings.Split(string(b), "\n") {
		var kb uint64
		if strings.HasPrefix(line, "MemAvailable:") {
			fmt.Sscanf(line[len("MemAvailable:"):], "%d", &kb)
			avail = kb << 10
		} else if strings.HasPrefix(line, "MemTotal:") {
			fmt.Sscanf(line[len("MemTotal:"):], "%d", &kb)
			total = kb << 10
		}
	}
	return avail, total, nil
}

// drainExec runs argv to completion and fails on a non-zero exit.
func drainExec(ctx context.Context, c *guest.Client, argv []string) error {
	st, err := c.Exec(ctx, &agentv1.ExecRequest{Argv: argv, TimeoutMs: 60000})
	if err != nil {
		return err
	}
	var stderr strings.Builder
	for {
		ev, err := st.Recv()
		if err == io.EOF {
			return errors.New("exec stream ended without exit status")
		}
		if err != nil {
			return err
		}
		stderr.Write(ev.GetStderr())
		if ex := ev.GetExit(); ex != nil {
			if ex.Code != 0 {
				return fmt.Errorf("%v exited %d: %s", argv, ex.Code, stderr.String())
			}
			return nil
		}
	}
}
