package firecracker_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	agentv1 "github.com/tmjoris/kiln/gen/kiln/agent/v1"
	"github.com/tmjoris/kiln/internal/firecracker"
	"github.com/tmjoris/kiln/internal/guest"
)

// Run with: KILN_FIRECRACKER=.tools/bin/firecracker go test ./internal/firecracker -run Integration -v
// Needs /dev/kvm and `make image`.
func artifacts(t *testing.T) (bin firecracker.Binary, kernel, rootfs string) {
	root, _ := filepath.Abs("../..")
	bin.Firecracker = os.Getenv("KILN_FIRECRACKER")
	kernel = filepath.Join(root, "build/kernel/vmlinux")
	rootfs = filepath.Join(root, "build/images/python/rootfs.ext4")
	for _, p := range []string{bin.Firecracker, kernel, rootfs, "/dev/kvm"} {
		if _, err := os.Stat(p); p == "" || err != nil {
			t.Skipf("integration artifact missing: %q", p)
		}
	}
	return
}

func run(t *testing.T, ctx context.Context, c *guest.Client, argv ...string) (string, int32) {
	t.Helper()
	st, err := c.Exec(ctx, &agentv1.ExecRequest{Argv: argv, TimeoutMs: 10000})
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	for {
		ev, err := st.Recv()
		if err == io.EOF {
			t.Fatal("stream ended without exit")
		}
		if err != nil {
			t.Fatal(err)
		}
		out.Write(ev.GetStdout())
		out.Write(ev.GetStderr())
		if ex := ev.GetExit(); ex != nil {
			return out.String(), ex.Code
		}
	}
}

func TestIntegrationBootSnapshotRestore(t *testing.T) {
	bin, kernel, rootfs := artifacts(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	dir := t.TempDir()

	t0 := time.Now()
	vm, err := firecracker.Boot(bin, firecracker.BootSpec{
		ID: "boot", Dir: filepath.Join(dir, "boot"), Kernel: kernel, Rootfs: rootfs, VCPUs: 1, MemMiB: 256,
	})
	if err != nil {
		t.Fatal(err)
	}
	c, err := guest.Connect(ctx, vm.DialAgent)
	if err != nil {
		log, _ := os.ReadFile(filepath.Join(vm.Dir, "firecracker.log"))
		t.Fatalf("connect: %v\n%s", err, log)
	}
	t.Logf("cold boot to agent ready: %v", time.Since(t0))

	out, code := run(t, ctx, c, "sh", "-c", "echo hi > /workspace/f && cat /workspace/f && python3 -c 'print(6*7)'")
	if code != 0 || out != "hi\n42\n" {
		t.Fatalf("exec: code=%d out=%q", code, out)
	}
	if out, code := run(t, ctx, c, "sh", "-c", "ls /sys/class/net"); code != 0 || out != "lo\n" {
		t.Fatalf("expected only loopback, got %q", out)
	}

	// Snapshot with no open guest connection, as the pool builder does.
	c.Close()
	state, mem := filepath.Join(dir, "vm.snap"), filepath.Join(dir, "mem.snap")
	if err := vm.Snapshot(ctx, state, mem); err != nil {
		t.Fatal(err)
	}
	vm.Stop()

	for i := 0; i < 3; i++ {
		t0 = time.Now()
		r, err := firecracker.Restore(ctx, bin, firecracker.RestoreSpec{
			ID: fmt.Sprint("r", i), Dir: filepath.Join(dir, fmt.Sprint("r", i)), Rootfs: rootfs, StatePath: state, MemPath: mem,
		})
		if err != nil {
			t.Fatal(err)
		}
		rc, err := guest.Connect(ctx, r.DialAgent)
		if err != nil {
			log, _ := os.ReadFile(filepath.Join(r.Dir, "firecracker.log"))
			t.Fatalf("restore connect: %v\n%s", err, log)
		}
		t.Logf("restore %d to agent ready: %v", i, time.Since(t0))
		// The workspace file from before the snapshot is part of guest RAM.
		if out, _ := run(t, ctx, rc, "cat", "/workspace/f"); out != "hi\n" {
			t.Fatalf("restored state lost: %q", out)
		}
		rc.Close()
		defer r.Stop()
	}
}
