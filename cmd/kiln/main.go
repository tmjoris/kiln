// kiln is the command-line client for kilnd.
//
//	kiln run -- python3 -c 'print(1+1)'
//	id=$(kiln create)
//	kiln exec $id -- sh -c 'pip list | head'
//	kiln rm $id
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/types/known/durationpb"

	kilnv1 "github.com/tmjoris/kiln/gen/kiln/v1"
	"github.com/tmjoris/kiln/internal/sandbox"
)

func usage() {
	fmt.Fprint(os.Stderr, `usage: kiln [-addr host:port] <command> [flags] [-- argv...]

commands:
  run    [-backend b] [-cold] -- argv   one-shot: create, exec, delete
  create [-backend b] [-cold]           print a new sandbox id
  exec   <id> -- argv                   stream a command's output
  put    <id> <local> <remote>          copy a file in
  get    <id> <remote>                  print a file
  ls                                    list sandboxes
  rm     <id>...                        delete sandboxes
  stats                                 pools and host memory
`)
	os.Exit(2)
}

func main() {
	addr := flag.String("addr", envOr("KILN_ADDR", "localhost:7070"), "kilnd address")
	flag.Usage = usage
	flag.Parse()
	if flag.NArg() < 1 {
		usage()
	}
	conn, err := grpc.NewClient(*addr, grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(256<<20)))
	check(err)
	c := kilnv1.NewKilnClient(conn)
	ctx := context.Background()
	cmd, args := flag.Arg(0), flag.Args()[1:]

	switch cmd {
	case "run":
		fs, create := createFlags(cmd)
		timeout := fs.Duration("timeout", time.Minute, "command timeout")
		fs.Parse(args)
		resp, err := c.Run(ctx, &kilnv1.RunRequest{
			Sandbox: create(),
			Exec:    &kilnv1.ExecRequest{Argv: argv(fs.Args()), Timeout: durationpb.New(*timeout)},
		})
		check(err)
		os.Stdout.Write(resp.Stdout)
		os.Stderr.Write(resp.Stderr)
		fmt.Fprintf(os.Stderr, "[%s in %v, exec %v]\n", mode(resp.Startup.Mode),
			resp.Startup.ReadyLatency.AsDuration().Round(10*time.Microsecond), resp.Exit.Duration.AsDuration().Round(10*time.Microsecond))
		os.Exit(int(resp.Exit.Code))
	case "create":
		fs, create := createFlags(cmd)
		fs.Parse(args)
		sb, err := c.CreateSandbox(ctx, create())
		check(err)
		fmt.Println(sb.Id)
		fmt.Fprintf(os.Stderr, "[%s in %v]\n", mode(sb.Startup.Mode), sb.Startup.ReadyLatency.AsDuration().Round(10*time.Microsecond))
	case "exec":
		if len(args) < 2 {
			usage()
		}
		st, err := c.Exec(ctx, &kilnv1.ExecRequest{SandboxId: args[0], Argv: argv(args[1:])})
		check(err)
		for {
			ev, err := st.Recv()
			check(err)
			os.Stdout.Write(ev.GetStdout())
			os.Stderr.Write(ev.GetStderr())
			if ex := ev.GetExit(); ex != nil {
				if ex.TimedOut {
					fmt.Fprintln(os.Stderr, "[timed out]")
				}
				os.Exit(int(ex.Code))
			}
		}
	case "put":
		if len(args) != 3 {
			usage()
		}
		b, err := os.ReadFile(args[1])
		check(err)
		_, err = c.WriteFile(ctx, &kilnv1.WriteFileRequest{SandboxId: args[0], Path: args[2], Content: b})
		check(err)
	case "get":
		if len(args) != 2 {
			usage()
		}
		r, err := c.ReadFile(ctx, &kilnv1.ReadFileRequest{SandboxId: args[0], Path: args[1]})
		check(err)
		os.Stdout.Write(r.Content)
	case "ls":
		r, err := c.ListSandboxes(ctx, &kilnv1.ListSandboxesRequest{})
		check(err)
		w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(w, "ID\tBACKEND\tTEMPLATE\tSHAPE\tSTART\tAGE")
		for _, sb := range r.Sandboxes {
			fmt.Fprintf(w, "%s\t%s\t%s\t%dx%dMiB\t%s %v\t%v\n", sb.Id, backend(sb.Backend), sb.Template,
				sb.Resources.Vcpus, sb.Resources.MemoryMib, mode(sb.Startup.Mode),
				sb.Startup.ReadyLatency.AsDuration().Round(time.Millisecond), time.Since(sb.CreatedAt.AsTime()).Round(time.Second))
		}
		w.Flush()
	case "rm":
		for _, id := range args {
			_, err := c.DeleteSandbox(ctx, &kilnv1.DeleteSandboxRequest{Id: id})
			check(err)
		}
	case "stats":
		r, err := c.Stats(ctx, &kilnv1.StatsRequest{})
		check(err)
		fmt.Printf("active sandboxes: %d\nhost memory: %.1f / %.1f GiB available\n", r.ActiveSandboxes,
			float64(r.HostMemAvailableBytes)/(1<<30), float64(r.HostMemTotalBytes)/(1<<30))
		for _, p := range r.Pools {
			fmt.Printf("pool %-32s ready %d/%d (starting %d)\n", p.Key, p.Ready, p.Target, p.Starting)
		}
	default:
		usage()
	}
}

func createFlags(name string) (*flag.FlagSet, func() *kilnv1.CreateSandboxRequest) {
	fs := flag.NewFlagSet(name, flag.ExitOnError)
	b := fs.String("backend", "", "firecracker or gvisor (default: server's)")
	tmpl := fs.String("template", "", "template (default: server's)")
	cold := fs.Bool("cold", false, "force a cold boot (no pool, no snapshot)")
	noPool := fs.Bool("no-pool", false, "bypass the warm pool (snapshot restore if available)")
	vcpus := fs.Uint("vcpus", 0, "vCPUs")
	mem := fs.Uint("mem-mib", 0, "memory MiB")
	return fs, func() *kilnv1.CreateSandboxRequest {
		req := &kilnv1.CreateSandboxRequest{Template: *tmpl, ForceColdBoot: *cold, SkipPool: *noPool,
			Resources: &kilnv1.Resources{Vcpus: uint32(*vcpus), MemoryMib: uint32(*mem)}}
		if *b != "" {
			bk, err := sandbox.ParseBackend(*b)
			check(err)
			req.Backend = bk
		}
		return req
	}
}

func argv(a []string) []string {
	if len(a) > 0 && a[0] == "--" {
		a = a[1:]
	}
	if len(a) == 0 {
		usage()
	}
	return a
}

func mode(m kilnv1.StartMode) string {
	return strings.ToLower(strings.TrimPrefix(m.String(), "START_MODE_"))
}

func backend(b kilnv1.Backend) string {
	return strings.ToLower(strings.TrimPrefix(b.String(), "BACKEND_"))
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func check(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "kiln:", err)
		os.Exit(1)
	}
}
