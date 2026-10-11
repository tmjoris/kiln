// kiln-agent runs inside every sandbox. In a Firecracker guest it is PID 1
// (kernel cmdline init=/kiln/agent) and listens on vsock; under gVisor it is
// the container's init and listens on a Unix socket bind-mounted from the host.
package main

import (
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
	"google.golang.org/grpc"
	"google.golang.org/grpc/keepalive"

	agentv1 "github.com/tmjoris/kiln/gen/kiln/agent/v1"
	"github.com/tmjoris/kiln/internal/agent"
	"github.com/tmjoris/kiln/internal/vsock"
)

func main() {
	listen := flag.String("listen", "vsock:"+strconv.Itoa(vsock.AgentPort), "vsock:<port> or unix:<path>")
	setup := flag.Bool("setup", os.Getpid() == 1 && os.Getenv("KILN_BACKEND") != "gvisor",
		"prepare the guest (overlay root, /proc, lo); default when PID 1 in a VM")
	flag.Parse()
	log.SetFlags(log.Lmicroseconds)
	log.SetPrefix("kiln-agent: ")

	if *setup {
		if err := agent.SetupGuest(); err != nil {
			fatal(err)
		}
	}
	lis, err := listenOn(*listen)
	if err != nil {
		fatal(err)
	}
	srv := grpc.NewServer(
		grpc.MaxRecvMsgSize(256<<20),
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{MinTime: 5 * time.Second, PermitWithoutStream: true}),
	)
	agentv1.RegisterAgentServer(srv, agent.NewServer())
	if err := srv.Serve(lis); err != nil {
		fatal(err)
	}
}

func listenOn(spec string) (net.Listener, error) {
	kind, arg, ok := strings.Cut(spec, ":")
	if !ok {
		return nil, fmt.Errorf("bad -listen %q", spec)
	}
	switch kind {
	case "vsock":
		port, err := strconv.ParseUint(arg, 10, 32)
		if err != nil {
			return nil, err
		}
		return vsock.Listen(uint32(port))
	case "unix":
		os.Remove(arg)
		return net.Listen("unix", arg)
	}
	return nil, fmt.Errorf("unknown listener kind %q", kind)
}

// PID 1 must never exit: the kernel would panic. Log and park instead so the
// host sees a dead agent and tears the VM down.
func fatal(err error) {
	log.Print(err)
	if os.Getpid() == 1 {
		for {
			unix.Pause()
		}
	}
	os.Exit(1)
}
