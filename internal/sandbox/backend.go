// Package sandbox owns sandbox lifecycle: backends that start isolated
// guests, warm pools in front of them, and the manager that hands them out.
package sandbox

import (
	"context"
	"fmt"

	kilnv1 "github.com/tmjoris/kiln/gen/kiln/v1"
	"github.com/tmjoris/kiln/internal/guest"
)

// Spec is the shape of a sandbox. Two sandboxes with equal Specs are
// interchangeable, which is what makes pooling and snapshots possible.
type Spec struct {
	Template string
	VCPUs    uint32
	MemMiB   uint32
}

func (s Spec) String() string { return fmt.Sprintf("%s/%dx%d", s.Template, s.VCPUs, s.MemMiB) }

// Instance is one running guest.
type Instance interface {
	Guest() *guest.Client
	// Pid of the host process that embodies the guest (for accounting).
	Pid() int
	// Exited closes if the guest dies on its own. May be nil (never fires).
	Exited() <-chan struct{}
	Destroy() error
}

type Backend interface {
	Kind() kilnv1.Backend
	// Resolve fills defaults and validates the template.
	Resolve(s Spec) (Spec, error)
	// Prepare does one-time, per-shape work (e.g. building a snapshot) so
	// later Starts are fast. Safe to call concurrently and repeatedly.
	Prepare(ctx context.Context, s Spec) error
	// Start launches a guest and returns once its agent answers. cold forces
	// the slow path even if a faster one is available.
	Start(ctx context.Context, id string, s Spec, cold bool) (Instance, kilnv1.StartMode, error)
}
