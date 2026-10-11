// kilnd is the Kiln host daemon: it runs sandboxes on this machine and
// serves the kiln.v1.Kiln gRPC API.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/reflection"

	kilnv1 "github.com/tmjoris/kiln/gen/kiln/v1"
	"github.com/tmjoris/kiln/internal/sandbox"
	"github.com/tmjoris/kiln/internal/server"
)

func main() {
	var (
		listen       = flag.String("listen", ":7070", "gRPC listen address")
		metricsAddr  = flag.String("metrics", ":9090", "Prometheus /metrics address (empty to disable)")
		dataDir      = flag.String("data-dir", defaultDataDir(), "state directory (VM dirs, snapshots)")
		imagesDir    = flag.String("images", "build/images", "template images: <dir>/<template>/rootfs.{ext4,tar}")
		kernel       = flag.String("kernel", "build/kernel/vmlinux", "guest kernel for Firecracker")
		fcBin        = flag.String("firecracker", "firecracker", "firecracker binary")
		runscBin     = flag.String("runsc", "runsc", "gVisor runsc binary")
		gvPlatform   = flag.String("gvisor-platform", "systrap", "gVisor platform: systrap or kvm")
		gvNoCgroups  = flag.Bool("gvisor-ignore-cgroups", false, "skip cgroups for gVisor (dev containers)")
		backends     = flag.String("backends", "firecracker", "comma-separated backends to enable: firecracker,gvisor")
		defBackend   = flag.String("default-backend", "firecracker", "backend used when a request does not choose")
		template     = flag.String("template", "python", "default template")
		vcpus        = flag.Uint("vcpus", 1, "default vCPUs per sandbox")
		memMiB       = flag.Uint("mem-mib", 512, "default memory per sandbox (MiB)")
		pools        = flag.String("pools", "", "warm pools, e.g. firecracker/python=8,gvisor/python=4")
		warmup       = flag.String("warmup", "python3 -c 'import json,os,re,subprocess,sys'", "shell command run in Firecracker templates before snapshotting")
		poolWarmup   = flag.String("pool-warmup", "python3 -c pass", "shell command run in each pooled guest before parking (empty to skip)")
		idle         = flag.Duration("idle-timeout", 30*time.Minute, "destroy sandboxes idle this long")
		maxSandboxes = flag.Int("max-sandboxes", 0, "admission limit on live guests (0 = none)")
		minAvailMiB  = flag.Uint64("min-mem-available-mib", 1024, "refuse new guests below this host MemAvailable")
	)
	flag.Parse()
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))

	def := sandbox.Spec{Template: *template, VCPUs: uint32(*vcpus), MemMiB: uint32(*memMiB)}
	var bs []sandbox.Backend
	for _, name := range strings.Split(*backends, ",") {
		switch strings.TrimSpace(name) {
		case "firecracker":
			b, err := sandbox.NewFirecracker(sandbox.FirecrackerConfig{
				Binary: lookPath(*fcBin), Kernel: *kernel, ImagesDir: *imagesDir,
				DataDir: *dataDir + "/firecracker", Default: def, Warmup: shellArgv(*warmup),
			})
			must(err)
			bs = append(bs, b)
		case "gvisor":
			b, err := sandbox.NewGVisor(sandbox.GVisorConfig{
				Runsc: lookPath(*runscBin), ImagesDir: *imagesDir, DataDir: *dataDir,
				Platform: *gvPlatform, IgnoreCgroups: *gvNoCgroups, Default: def,
			})
			must(err)
			bs = append(bs, b)
		default:
			must(fmt.Errorf("unknown backend %q", name))
		}
	}
	db, err := sandbox.ParseBackend(*defBackend)
	must(err)
	poolCfg, err := parsePools(*pools)
	must(err)

	m, err := sandbox.NewManager(sandbox.Config{
		Backends: bs, DefaultBackend: db, Pools: poolCfg, IdleTimeout: *idle,
		MaxSandboxes: *maxSandboxes, MinMemAvailable: *minAvailMiB << 20,
		PoolWarmup: shellArgv(*poolWarmup),
	})
	must(err)

	gs := grpc.NewServer(grpc.MaxRecvMsgSize(256 << 20))
	kilnv1.RegisterKilnServer(gs, server.New(m))
	hs := health.NewServer()
	healthpb.RegisterHealthServer(gs, hs)
	reflection.Register(gs)

	lis, err := net.Listen("tcp", *listen)
	must(err)
	if *metricsAddr != "" {
		go func() {
			mux := http.NewServeMux()
			mux.Handle("/metrics", promhttp.Handler())
			slog.Error("metrics server", "err", http.ListenAndServe(*metricsAddr, mux))
		}()
	}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sig
		slog.Info("shutting down")
		hs.Shutdown()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		go func() { <-ctx.Done(); gs.Stop() }()
		gs.GracefulStop()
	}()
	slog.Info("kilnd listening", "addr", lis.Addr().String(), "backends", *backends, "pools", *pools)
	err = gs.Serve(lis)
	m.Close()
	if err != nil {
		must(err)
	}
}

func shellArgv(cmd string) []string {
	if cmd == "" {
		return nil
	}
	return []string{"sh", "-c", cmd}
}

func parsePools(s string) (map[string]int, error) {
	out := map[string]int{}
	if s == "" {
		return out, nil
	}
	for _, kv := range strings.Split(s, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(kv), "=")
		n, err := strconv.Atoi(v)
		if !ok || err != nil || n < 0 {
			return nil, fmt.Errorf("bad pool %q (want backend/template=N)", kv)
		}
		out[k] = n
	}
	return out, nil
}

func defaultDataDir() string {
	if os.Geteuid() == 0 {
		return "/var/lib/kiln"
	}
	if d, err := os.UserCacheDir(); err == nil {
		return d + "/kiln"
	}
	return "/tmp/kiln"
}

func lookPath(p string) string {
	if strings.Contains(p, "/") {
		return p
	}
	for _, dir := range strings.Split(os.Getenv("PATH"), ":") {
		if _, err := os.Stat(dir + "/" + p); err == nil {
			return dir + "/" + p
		}
	}
	return p
}

func must(err error) {
	if err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}
