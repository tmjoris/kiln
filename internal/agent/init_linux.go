package agent

import (
	"encoding/binary"
	"fmt"
	"os"
	"unsafe"

	"golang.org/x/sys/unix"
)

// SetupGuest turns a freshly booted microVM into a usable workspace. The
// kernel mounts the shared rootfs read-only; we stack a tmpfs overlay on top
// and switch into it, so every sandbox gets a private writable root without
// a per-VM disk copy. Writes cost guest RAM, which is already capped.
func SetupGuest() error {
	const base = "/mnt/kiln"
	steps := []struct {
		what string
		fn   func() error
	}{
		{"mount scratch tmpfs", func() error { return unix.Mount("tmpfs", base, "tmpfs", 0, "mode=0755") }},
		{"mkdir overlay dirs", func() error {
			for _, d := range []string{"upper", "work", "root"} {
				if err := os.Mkdir(base+"/"+d, 0o755); err != nil {
					return err
				}
			}
			return nil
		}},
		{"mount overlay", func() error {
			return unix.Mount("overlay", base+"/root", "overlay", 0,
				"lowerdir=/,upperdir="+base+"/upper,workdir="+base+"/work")
		}},
		{"move /dev", func() error { return unix.Mount("/dev", base+"/root/dev", "", unix.MS_MOVE, "") }},
		{"switch root", func() error {
			if err := unix.Chdir(base + "/root"); err != nil {
				return err
			}
			if err := unix.Mount(".", "/", "", unix.MS_MOVE, ""); err != nil {
				return err
			}
			if err := unix.Chroot("."); err != nil {
				return err
			}
			return unix.Chdir("/")
		}},
	}
	for _, s := range steps {
		if err := s.fn(); err != nil {
			return fmt.Errorf("%s: %w", s.what, err)
		}
	}
	mounts := []struct{ src, dst, fstype, data string }{
		{"proc", "/proc", "proc", ""},
		{"sysfs", "/sys", "sysfs", ""},
		{"devpts", "/dev/pts", "devpts", "newinstance,ptmxmode=0666"},
		{"tmpfs", "/dev/shm", "tmpfs", "mode=1777"},
		{"tmpfs", "/run", "tmpfs", "mode=0755"},
		{"cgroup2", "/sys/fs/cgroup", "cgroup2", ""},
	}
	for _, m := range mounts {
		os.MkdirAll(m.dst, 0o755)
		if err := unix.Mount(m.src, m.dst, m.fstype, unix.MS_NOSUID, m.data); err != nil {
			return fmt.Errorf("mount %s: %w", m.dst, err)
		}
	}
	os.MkdirAll(defaultCwd, 0o755)
	os.Symlink("/proc/self/fd", "/dev/fd")
	return loopbackUp()
}

// loopbackUp enables lo. There is no other NIC: agents can still run and test
// local servers, but nothing leaves the VM.
func loopbackUp() error {
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	ifr, err := unix.NewIfreq("lo")
	if err != nil {
		return err
	}
	if err := unix.IoctlIfreq(fd, unix.SIOCGIFFLAGS, ifr); err != nil {
		return err
	}
	ifr.SetUint16(ifr.Uint16() | unix.IFF_UP)
	return unix.IoctlIfreq(fd, unix.SIOCSIFFLAGS, ifr)
}

// addEntropy credits host-supplied randomness to the guest pool
// (RNDADDENTROPY), so snapshot clones diverge immediately.
func addEntropy(b []byte) error {
	f, err := os.OpenFile("/dev/urandom", os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	buf := make([]byte, 8+len(b))
	binary.LittleEndian.PutUint32(buf[0:], uint32(len(b)*8))
	binary.LittleEndian.PutUint32(buf[4:], uint32(len(b)))
	copy(buf[8:], b)
	_, _, errno := unix.Syscall(unix.SYS_IOCTL, f.Fd(), unix.RNDADDENTROPY, uintptr(unsafe.Pointer(&buf[0])))
	if errno != 0 {
		return errno
	}
	return nil
}
