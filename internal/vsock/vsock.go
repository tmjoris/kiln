// Package vsock implements the two ends of the host<->guest channel.
//
// Guest side: an AF_VSOCK listener wrapped as a net.Listener, driven by Go's
// netpoller. Host side: Firecracker exposes guest vsock ports through a Unix
// socket using a tiny text handshake ("CONNECT <port>\n" -> "OK <n>\n").
package vsock

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// AgentPort is the vsock port kiln-agent listens on inside a microVM.
const AgentPort = 52

type addr struct{ cid, port uint32 }

func (a addr) Network() string { return "vsock" }
func (a addr) String() string  { return fmt.Sprintf("vsock(%d:%d)", a.cid, a.port) }

type listener struct {
	f    *os.File
	rc   syscall.RawConn
	port uint32
}

// Listen opens a vsock listener on port for any CID.
func Listen(port uint32) (net.Listener, error) {
	fd, err := unix.Socket(unix.AF_VSOCK, unix.SOCK_STREAM|unix.SOCK_CLOEXEC|unix.SOCK_NONBLOCK, 0)
	if err != nil {
		return nil, fmt.Errorf("vsock socket: %w", err)
	}
	if err := unix.Bind(fd, &unix.SockaddrVM{CID: unix.VMADDR_CID_ANY, Port: port}); err != nil {
		unix.Close(fd)
		return nil, fmt.Errorf("vsock bind: %w", err)
	}
	if err := unix.Listen(fd, 128); err != nil {
		unix.Close(fd)
		return nil, fmt.Errorf("vsock listen: %w", err)
	}
	f := os.NewFile(uintptr(fd), "vsock-listener")
	rc, err := f.SyscallConn()
	if err != nil {
		f.Close()
		return nil, err
	}
	return &listener{f: f, rc: rc, port: port}, nil
}

func (l *listener) Accept() (net.Conn, error) {
	var (
		nfd int
		sa  unix.Sockaddr
		aerr error
	)
	err := l.rc.Read(func(fd uintptr) bool {
		nfd, sa, aerr = unix.Accept4(int(fd), unix.SOCK_CLOEXEC|unix.SOCK_NONBLOCK)
		return aerr != unix.EAGAIN
	})
	if err != nil {
		return nil, err
	}
	if aerr != nil {
		return nil, aerr
	}
	remote := addr{}
	if vm, ok := sa.(*unix.SockaddrVM); ok {
		remote = addr{vm.CID, vm.Port}
	}
	return &conn{File: os.NewFile(uintptr(nfd), "vsock-conn"), local: addr{unix.VMADDR_CID_ANY, l.port}, remote: remote}, nil
}

func (l *listener) Close() error   { return l.f.Close() }
func (l *listener) Addr() net.Addr { return addr{unix.VMADDR_CID_ANY, l.port} }

type conn struct {
	*os.File
	local, remote addr
}

func (c *conn) LocalAddr() net.Addr  { return c.local }
func (c *conn) RemoteAddr() net.Addr { return c.remote }

// ErrNotListening means Firecracker reached the guest but nothing accepted
// the connection yet (agent still booting).
var ErrNotListening = errors.New("vsock: guest port not listening")

// DialFirecracker connects to a guest vsock port through the Unix socket
// Firecracker creates for the VM's vsock device.
func DialFirecracker(ctx context.Context, udsPath string, port uint32) (net.Conn, error) {
	var d net.Dialer
	c, err := d.DialContext(ctx, "unix", udsPath)
	if err != nil {
		return nil, err
	}
	if dl, ok := ctx.Deadline(); ok {
		c.SetDeadline(dl)
	}
	if _, err := fmt.Fprintf(c, "CONNECT %d\n", port); err != nil {
		c.Close()
		return nil, err
	}
	// Read the reply one byte at a time: anything past the newline belongs to
	// the gRPC stream and must not be swallowed by a buffer.
	var line strings.Builder
	b := make([]byte, 1)
	for {
		if _, err := c.Read(b); err != nil {
			c.Close()
			return nil, ErrNotListening
		}
		if b[0] == '\n' {
			break
		}
		line.WriteByte(b[0])
		if line.Len() > 64 {
			c.Close()
			return nil, fmt.Errorf("vsock: bad handshake %q", line.String())
		}
	}
	if !strings.HasPrefix(line.String(), "OK ") {
		c.Close()
		return nil, fmt.Errorf("vsock: handshake %q", line.String())
	}
	c.SetDeadline(time.Time{})
	return c, nil
}
