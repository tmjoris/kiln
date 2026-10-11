package sandbox

import (
	"context"
	"log/slog"
	"sync/atomic"
	"time"

	kilnv1 "github.com/tmjoris/kiln/gen/kiln/v1"
)

type pooled struct {
	id   string
	inst Instance
	mode kilnv1.StartMode
}

// pool keeps up to target ready guests of one shape. Handing one out is a
// channel receive; the refill happens off the request path.
type pool struct {
	m       *Manager
	backend Backend
	spec    Spec
	target  int
	ready   chan pooled
	// starting counts guests being started for this pool right now.
	starting atomic.Int32
	kick     chan struct{}
}

func newPool(m *Manager, b Backend, s Spec, target int) *pool {
	return &pool{m: m, backend: b, spec: s, target: target, ready: make(chan pooled, target), kick: make(chan struct{}, 1)}
}

func (p *pool) key() string { return poolKey(p.backend.Kind(), p.spec) }

// get returns a ready guest without blocking, or false on a pool miss.
func (p *pool) get() (pooled, bool) {
	defer p.signal()
	for {
		select {
		case g := <-p.ready:
			// A guest can die while parked (OOM, host trouble); skip it.
			if ex := g.inst.Exited(); ex != nil {
				select {
				case <-ex:
					go func() { g.inst.Destroy(); p.m.release() }()
					continue
				default:
				}
			}
			return g, true
		default:
			return pooled{}, false
		}
	}
}

func (p *pool) signal() {
	select {
	case p.kick <- struct{}{}:
	default:
	}
}

func (p *pool) run(ctx context.Context) {
	for {
		err := p.backend.Prepare(ctx, p.spec)
		if err == nil {
			break
		}
		slog.Error("pool prepare failed; retrying", "pool", p.key(), "err", err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(5 * time.Second):
		}
	}
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		p.fill(ctx)
		select {
		case <-ctx.Done():
			p.drain()
			return
		case <-p.kick:
		case <-tick.C:
		}
	}
}

func (p *pool) fill(ctx context.Context) {
	for len(p.ready)+int(p.starting.Load()) < p.target {
		// Bound host-wide concurrent starts: boots are CPU-bound and a
		// thundering refill would slow down on-demand creates.
		select {
		case p.m.startSem <- struct{}{}:
		default:
			return
		}
		if !p.m.admit() {
			<-p.m.startSem
			return
		}
		p.starting.Add(1)
		go func() {
			defer func() { <-p.m.startSem; p.starting.Add(-1); p.signal() }()
			id := newID()
			ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
			defer cancel()
			inst, mode, err := p.backend.Start(ctx, id, p.spec, false)
			if err == nil && len(p.m.cfg.PoolWarmup) > 0 {
				if err = drainExec(ctx, inst.Guest(), p.m.cfg.PoolWarmup); err != nil {
					inst.Destroy()
				}
			}
			if err != nil {
				slog.Warn("pool start failed", "pool", p.key(), "err", err)
				p.m.release()
				time.Sleep(time.Second)
				return
			}
			p.ready <- pooled{id: id, inst: inst, mode: mode}
		}()
	}
}

func (p *pool) drain() {
	for {
		select {
		case g := <-p.ready:
			g.inst.Destroy()
			p.m.release()
		default:
			return
		}
	}
}
