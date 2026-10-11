// kiln-bench measures a running kilnd: startup latency per start mode,
// concurrent start throughput, density (host memory per sandbox) and exec
// round-trip latency. Results are written as JSON for bench/report.py.
//
// Run it on the same host as kilnd: density reads /proc to attribute memory.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/types/known/durationpb"

	kilnv1 "github.com/tmjoris/kiln/gen/kiln/v1"
	"github.com/tmjoris/kiln/internal/firecracker"
	"github.com/tmjoris/kiln/internal/sandbox"
)

var (
	addr    = flag.String("addr", "localhost:7070", "kilnd address")
	backend = flag.String("backend", "firecracker", "backend under test")
	outDir  = flag.String("out", "bench/results", "directory for JSON results")
	n       = flag.Int("n", 50, "samples per configuration")
	probe   = flag.String("probe", "python3 -c pass", "first command run in each sandbox (time-to-first-exec)")
	maxN    = flag.Int("density-max", 100, "density: stop after this many sandboxes")
	floor   = flag.Uint64("density-floor-mib", 3072, "density: stop when host MemAvailable drops below this")
	levels  = flag.String("concurrency", "1,4,8,16,32", "concurrency levels for the burst experiment")
	modes   = flag.String("modes", "cold_boot,snapshot_restore,warm_pool", "start modes for the startup experiment")
)

type client struct {
	kilnv1.KilnClient
	bk kilnv1.Backend
}

func main() {
	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: kiln-bench [flags] startup|burst|density|exec|all")
		flag.PrintDefaults()
	}
	flag.Parse()
	if flag.NArg() != 1 {
		flag.Usage()
		os.Exit(2)
	}
	conn, err := grpc.NewClient(*addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	check(err)
	bk, err := sandbox.ParseBackend(*backend)
	check(err)
	c := &client{KilnClient: kilnv1.NewKilnClient(conn), bk: bk}
	check(os.MkdirAll(*outDir, 0o755))

	exps := map[string]func(*client) any{"startup": startup, "burst": burst, "density": density, "exec": execLatency}
	names := []string{flag.Arg(0)}
	if names[0] == "all" {
		names = []string{"exec", "startup", "burst", "density"}
	}
	for _, name := range names {
		fn, ok := exps[name]
		if !ok {
			flag.Usage()
			os.Exit(2)
		}
		log.Printf("== %s (%s)", name, *backend)
		res := map[string]any{
			"experiment": name, "backend": *backend, "time": time.Now().UTC(),
			"host": hostInfo(), "results": fn(c),
		}
		path := filepath.Join(*outDir, fmt.Sprintf("%s-%s.json", name, *backend))
		b, _ := json.MarshalIndent(res, "", "  ")
		check(os.WriteFile(path, b, 0o644))
		log.Printf("wrote %s", path)
	}
}

// --- startup -----------------------------------------------------------

type startSample struct {
	Mode       string  `json:"mode"`
	CreateMs   float64 `json:"create_ms"`     // client-observed CreateSandbox RTT
	ReadyMs    float64 `json:"ready_ms"`      // server-side ready latency
	FirstExec  float64 `json:"first_exec_ms"` // first probe exec RTT
	SecondExec float64 `json:"second_exec_ms"`
	// TTFX: request start until the first probe finished; what a caller feels.
	TTFXMs float64 `json:"ttfx_ms"`
}

func startup(c *client) any {
	var out []startSample
	for _, mode := range strings.Split(*modes, ",") {
		req := &kilnv1.CreateSandboxRequest{Backend: c.bk}
		switch mode {
		case "cold_boot":
			req.ForceColdBoot = true
		case "snapshot_restore":
			req.SkipPool = true
		case "warm_pool":
		default:
			log.Fatalf("unknown mode %q", mode)
		}
		var lat []float64
		for i := 0; i < *n; i++ {
			if mode == "warm_pool" {
				waitPoolFull(c)
			}
			s, err := c.startOnce(req)
			if err != nil {
				log.Printf("%s sample %d: %v", mode, i, err)
				continue
			}
			if s.Mode != mode {
				log.Printf("%s sample %d: got mode %s (pool empty or no snapshot?)", mode, i, s.Mode)
			}
			out = append(out, s)
			lat = append(lat, s.TTFXMs)
		}
		log.Printf("%-17s ttfx p50 %6.1fms p99 %6.1fms", mode, pct(lat, 50), pct(lat, 99))
	}
	return out
}

func (c *client) startOnce(req *kilnv1.CreateSandboxRequest) (startSample, error) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	t0 := time.Now()
	sb, err := c.CreateSandbox(ctx, req)
	if err != nil {
		return startSample{}, err
	}
	created := time.Since(t0)
	defer c.DeleteSandbox(context.Background(), &kilnv1.DeleteSandboxRequest{Id: sb.Id})
	first, err := c.run(ctx, sb.Id, *probe)
	if err != nil {
		return startSample{}, err
	}
	ttfx := time.Since(t0)
	second, err := c.run(ctx, sb.Id, *probe)
	if err != nil {
		return startSample{}, err
	}
	return startSample{
		Mode:     strings.ToLower(strings.TrimPrefix(sb.Startup.Mode.String(), "START_MODE_")),
		CreateMs: ms(created), ReadyMs: ms(sb.Startup.ReadyLatency.AsDuration()),
		FirstExec: ms(first), SecondExec: ms(second), TTFXMs: ms(ttfx),
	}, nil
}

// run executes a shell command and returns the RTT; non-zero exit is an error.
func (c *client) run(ctx context.Context, id, cmd string) (time.Duration, error) {
	t0 := time.Now()
	st, err := c.Exec(ctx, &kilnv1.ExecRequest{SandboxId: id, Argv: []string{"sh", "-c", cmd}, Timeout: durationpb.New(30 * time.Second)})
	if err != nil {
		return 0, err
	}
	for {
		ev, err := st.Recv()
		if err == io.EOF {
			return 0, fmt.Errorf("no exit status")
		}
		if err != nil {
			return 0, err
		}
		if ex := ev.GetExit(); ex != nil {
			if ex.Code != 0 {
				return 0, fmt.Errorf("probe exited %d", ex.Code)
			}
			return time.Since(t0), nil
		}
	}
}

func waitPoolFull(c *client) {
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		st, err := c.Stats(context.Background(), &kilnv1.StatsRequest{})
		check(err)
		full := false
		for _, p := range st.Pools {
			if strings.HasPrefix(p.Key, *backend+"/") && p.Ready == p.Target && p.Target > 0 {
				full = true
			}
		}
		if full {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	log.Printf("warning: pool did not refill within 30s")
}

// --- burst -------------------------------------------------------------

type burstResult struct {
	Concurrency int       `json:"concurrency"`
	Mode        string    `json:"mode"`
	WallMs      float64   `json:"wall_ms"`
	PerSec      float64   `json:"sandboxes_per_sec"`
	TTFXMs      []float64 `json:"ttfx_ms"`
	Errors      int       `json:"errors"`
}

// burst fires C simultaneous creates (bypassing the pool, so this measures
// the restore path under contention) and records each one's TTFX.
func burst(c *client) any {
	var out []burstResult
	for _, lvl := range strings.Split(*levels, ",") {
		var conc int
		fmt.Sscan(lvl, &conc)
		for _, mode := range []string{"snapshot_restore", "cold_boot"} {
			req := &kilnv1.CreateSandboxRequest{Backend: c.bk, SkipPool: true, ForceColdBoot: mode == "cold_boot"}
			r := burstResult{Concurrency: conc, Mode: mode}
			var mu sync.Mutex
			var wg sync.WaitGroup
			t0 := time.Now()
			for i := 0; i < conc; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					s, err := c.startOnce(req)
					mu.Lock()
					defer mu.Unlock()
					if err != nil {
						r.Errors++
						log.Printf("burst: %v", err)
						return
					}
					r.TTFXMs = append(r.TTFXMs, s.TTFXMs)
				}()
			}
			wg.Wait()
			r.WallMs = ms(time.Since(t0))
			r.PerSec = float64(len(r.TTFXMs)) / time.Since(t0).Seconds()
			log.Printf("c=%-3d %-17s %6.1f/s  ttfx p50 %6.1fms p99 %6.1fms  errors %d",
				conc, mode, r.PerSec, pct(r.TTFXMs, 50), pct(r.TTFXMs, 99), r.Errors)
			out = append(out, r)
			time.Sleep(time.Second) // let teardown finish before the next level
		}
	}
	return out
}

// --- density -----------------------------------------------------------

type densityPoint struct {
	N           int     `json:"n"`
	MemUsedMiB  float64 `json:"host_mem_used_mib"` // MemAvailable delta since baseline
	SumPSSMiB   float64 `json:"sum_pss_mib"`       // PSS of all guest processes
	SumRSSMiB   float64 `json:"sum_rss_mib"`
	CreateMs    float64 `json:"create_ms"`
	FirstExecMs float64 `json:"first_exec_ms"`
}

type densityResult struct {
	Mode   string         `json:"mode"`
	Points []densityPoint `json:"points"`
	Stop   string         `json:"stop_reason"`
}

// density starts sandboxes one at a time and keeps them alive, each having
// run the probe once (an "agent that has done a little work"), recording host
// memory after each. It stops at -density-max or the MemAvailable floor.
func density(c *client) any {
	var out []densityResult
	dmodes := []string{"snapshot_restore", "cold_boot"}
	if c.bk == kilnv1.Backend_BACKEND_GVISOR {
		dmodes = []string{"cold_boot"}
	}
	for _, mode := range dmodes {
		runtime.GC()
		time.Sleep(2 * time.Second)
		base, _, _ := sandbox.HostMemory()
		// Pooled guests already exist; attribute only what we add.
		basePSS, baseRSS := guestMemory(c.bk)
		res := densityResult{Mode: mode}
		var ids []string
		req := &kilnv1.CreateSandboxRequest{Backend: c.bk, SkipPool: true, ForceColdBoot: mode == "cold_boot"}
		for i := 1; ; i++ {
			avail, _, _ := sandbox.HostMemory()
			if avail < *floor<<20 {
				res.Stop = fmt.Sprintf("host MemAvailable below %d MiB", *floor)
				break
			}
			if i > *maxN {
				res.Stop = fmt.Sprintf("reached %d sandboxes", *maxN)
				break
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			t0 := time.Now()
			sb, err := c.CreateSandbox(ctx, req)
			if err != nil {
				cancel()
				res.Stop = "create failed: " + err.Error()
				break
			}
			created := time.Since(t0)
			ids = append(ids, sb.Id)
			first, err := c.run(ctx, sb.Id, *probe)
			cancel()
			if err != nil {
				res.Stop = "probe failed: " + err.Error()
				break
			}
			if i%5 == 0 || i == 1 {
				time.Sleep(200 * time.Millisecond) // let the guest settle
				avail, _, _ = sandbox.HostMemory()
				pss, rss := guestMemory(c.bk)
				pss, rss = pss-min(pss, basePSS), rss-min(rss, baseRSS)
				p := densityPoint{N: i, MemUsedMiB: (float64(base) - float64(avail)) / (1 << 20), SumPSSMiB: mib(pss), SumRSSMiB: mib(rss),
					CreateMs: ms(created), FirstExecMs: ms(first)}
				res.Points = append(res.Points, p)
				log.Printf("%s n=%-3d host used %7.1f MiB (%.1f/sandbox)  ΣPSS %7.1f MiB (%.1f/sandbox)",
					mode, i, p.MemUsedMiB, p.MemUsedMiB/float64(i), p.SumPSSMiB, p.SumPSSMiB/float64(i))
			}
		}
		log.Printf("%s stopped: %s", mode, res.Stop)
		var wg sync.WaitGroup
		for _, id := range ids {
			wg.Add(1)
			go func() {
				defer wg.Done()
				c.DeleteSandbox(context.Background(), &kilnv1.DeleteSandboxRequest{Id: id})
			}()
		}
		wg.Wait()
		out = append(out, res)
	}
	return out
}

// guestMemory sums PSS/RSS over every process that embodies a guest:
// firecracker for microVMs, the runsc sandbox and gofer for gVisor.
func guestMemory(bk kilnv1.Backend) (pss, rss uint64) {
	ents, _ := os.ReadDir("/proc")
	for _, e := range ents {
		var pid int
		if _, err := fmt.Sscan(e.Name(), &pid); err != nil {
			continue
		}
		comm, err := os.ReadFile(fmt.Sprintf("/proc/%d/comm", pid))
		if err != nil {
			continue
		}
		name := strings.TrimSpace(string(comm))
		match := name == "firecracker" || strings.HasPrefix(name, "fc_vcpu")
		if bk == kilnv1.Backend_BACKEND_GVISOR {
			match = strings.HasPrefix(name, "runsc") || name == "exe"
		}
		if !match {
			continue
		}
		r, p, err := firecracker.ProcessMemory(pid)
		if err == nil {
			pss += p
			rss += r
		}
	}
	return pss, rss
}

// --- exec --------------------------------------------------------------

type execResult struct {
	Command string    `json:"command"`
	RTTMs   []float64 `json:"rtt_ms"`
}

func execLatency(c *client) any {
	ctx := context.Background()
	sb, err := c.CreateSandbox(ctx, &kilnv1.CreateSandboxRequest{Backend: c.bk})
	check(err)
	defer c.DeleteSandbox(ctx, &kilnv1.DeleteSandboxRequest{Id: sb.Id})
	var out []execResult
	for _, cmd := range []string{"true", "python3 -c pass", "echo hi > f && cat f"} {
		r := execResult{Command: cmd}
		for i := 0; i < *n*4; i++ {
			d, err := c.run(ctx, sb.Id, cmd)
			check(err)
			r.RTTMs = append(r.RTTMs, ms(d))
		}
		log.Printf("%-22q p50 %6.2fms p99 %6.2fms", cmd, pct(r.RTTMs, 50), pct(r.RTTMs, 99))
		out = append(out, r)
	}
	return out
}

// --- helpers -----------------------------------------------------------

func hostInfo() map[string]any {
	_, total, _ := sandbox.HostMemory()
	model := ""
	if b, err := os.ReadFile("/proc/cpuinfo"); err == nil {
		for _, l := range strings.Split(string(b), "\n") {
			if strings.HasPrefix(l, "model name") {
				_, model, _ = strings.Cut(l, ": ")
				break
			}
		}
	}
	kernel, _ := os.ReadFile("/proc/sys/kernel/osrelease")
	return map[string]any{"cpus": runtime.NumCPU(), "cpu_model": model, "mem_total_mib": mib(total),
		"kernel": strings.TrimSpace(string(kernel))}
}

func pct(xs []float64, p float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	s := append([]float64(nil), xs...)
	sort.Float64s(s)
	i := int(float64(len(s)-1) * p / 100)
	return s[i]
}

func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }
func mib(b uint64) float64       { return float64(b) / (1 << 20) }

func check(err error) {
	if err != nil {
		log.Fatal(err)
	}
}
