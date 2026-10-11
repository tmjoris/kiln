package vsock

import (
	"context"
	"fmt"
	"net"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// DialUnix connects to a Unix socket, even when its path exceeds the
// kernel's 108-byte sun_path limit: long paths are reached through an
// O_PATH handle on the parent directory (/proc/self/fd/N/name).
func DialUnix(ctx context.Context, path string) (net.Conn, error) {
	var d net.Dialer
	if len(path) < 100 {
		return d.DialContext(ctx, "unix", path)
	}
	fd, err := unix.Open(filepath.Dir(path), unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	defer unix.Close(fd)
	return d.DialContext(ctx, "unix", fmt.Sprintf("/proc/self/fd/%d/%s", fd, filepath.Base(path)))
}
