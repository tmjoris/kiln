// Package guest connects the host to a sandbox's in-guest agent.
package guest

import (
	"context"
	"errors"
	"net"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/backoff"
	"google.golang.org/grpc/credentials/insecure"

	agentv1 "github.com/tmjoris/kiln/gen/kiln/agent/v1"
)

// DialFunc opens one raw stream to the agent (vsock via Firecracker, or a
// Unix socket for gVisor).
type DialFunc func(ctx context.Context) (net.Conn, error)

type Client struct {
	agentv1.AgentClient
	conn *grpc.ClientConn
}

func (c *Client) Close() error { return c.conn.Close() }

// pollInterval bounds how stale readiness detection can be. Startup is the
// metric Kiln optimises, so poll tightly; a failed attempt costs a few µs.
const pollInterval = 500 * time.Microsecond

// Connect waits until the agent accepts a connection, then returns a gRPC
// client that reuses that first connection. gRPC's own reconnect backoff
// starts at 1s, which would dominate a ~100ms boot, so readiness is detected
// here with raw dials instead.
func Connect(ctx context.Context, dial DialFunc) (*Client, error) {
	var first net.Conn
	for {
		c, err := dial(ctx)
		if err == nil {
			first = c
			break
		}
		select {
		case <-ctx.Done():
			return nil, errors.Join(ctx.Err(), err)
		case <-time.After(pollInterval):
		}
	}
	var once sync.Once
	dialer := func(ctx context.Context, _ string) (net.Conn, error) {
		var c net.Conn
		once.Do(func() { c, first = first, nil })
		if c != nil {
			return c, nil
		}
		return dial(ctx)
	}
	conn, err := grpc.NewClient("passthrough:///agent",
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(dialer),
		grpc.WithConnectParams(grpc.ConnectParams{
			Backoff:           backoff.Config{BaseDelay: 5 * time.Millisecond, Multiplier: 1.6, MaxDelay: time.Second},
			MinConnectTimeout: 2 * time.Second,
		}),
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(256<<20), grpc.MaxCallSendMsgSize(256<<20)),
	)
	if err != nil {
		first.Close()
		return nil, err
	}
	cl := &Client{AgentClient: agentv1.NewAgentClient(conn), conn: conn}
	if _, err := cl.Ping(ctx, &agentv1.PingRequest{}, grpc.WaitForReady(true)); err != nil {
		cl.Close()
		return nil, err
	}
	return cl, nil
}
