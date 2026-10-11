// Package agent is the in-sandbox half of Kiln: a gRPC server that runs
// commands and moves files on behalf of the host.
package agent

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"golang.org/x/sys/unix"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"

	agentv1 "github.com/tmjoris/kiln/gen/kiln/agent/v1"
	kilnv1 "github.com/tmjoris/kiln/gen/kiln/v1"
)

const (
	defaultCwd       = "/workspace"
	defaultTimeout   = 5 * time.Minute
	defaultMaxOutput = 8 << 20
	defaultMaxRead   = 64 << 20
	chunkSize        = 32 << 10
	// After the main process exits, background children may still hold the
	// output pipes open. Drain for at most this long, then stop reading.
	drainGrace = 50 * time.Millisecond
)

var baseEnv = map[string]string{
	"PATH": "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
	"HOME": "/root",
	"LANG": "C.UTF-8",
	"TERM": "dumb",
}

type Server struct {
	agentv1.UnimplementedAgentServer
	bootID uint64
	reaper *reaper
}

func NewServer() *Server {
	var b [8]byte
	rand.Read(b[:])
	return &Server{bootID: binary.LittleEndian.Uint64(b[:]), reaper: newReaper()}
}

func (s *Server) Ping(context.Context, *agentv1.PingRequest) (*agentv1.PingResponse, error) {
	return &agentv1.PingResponse{BootId: s.bootID}, nil
}

func (s *Server) Init(_ context.Context, req *agentv1.InitRequest) (*agentv1.InitResponse, error) {
	// Best effort: gVisor owns the clock and refuses these, which is fine
	// because its clock already tracks the host.
	if req.UnixNanos != 0 {
		tv := unix.NsecToTimeval(req.UnixNanos)
		_ = unix.Settimeofday(&tv)
	}
	if len(req.Entropy) > 0 {
		_ = addEntropy(req.Entropy)
	}
	if req.Hostname != "" {
		_ = unix.Sethostname([]byte(req.Hostname))
	}
	return &agentv1.InitResponse{}, nil
}

func (s *Server) WriteFile(_ context.Context, req *agentv1.WriteFileRequest) (*agentv1.WriteFileResponse, error) {
	p, err := absPath(req.Path)
	if err != nil {
		return nil, err
	}
	mode := fs.FileMode(req.Mode)
	if mode == 0 {
		mode = 0o644
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	if err := os.WriteFile(p, req.Content, mode); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &agentv1.WriteFileResponse{}, os.Chmod(p, mode)
}

func (s *Server) ReadFile(_ context.Context, req *agentv1.ReadFileRequest) (*agentv1.ReadFileResponse, error) {
	p, err := absPath(req.Path)
	if err != nil {
		return nil, err
	}
	max := req.MaxBytes
	if max == 0 {
		max = defaultMaxRead
	}
	f, err := os.Open(p)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, status.Error(codes.NotFound, err.Error())
	} else if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, int64(max)+1))
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	if uint64(len(b)) > max {
		return nil, status.Errorf(codes.ResourceExhausted, "file larger than %d bytes", max)
	}
	return &agentv1.ReadFileResponse{Content: b}, nil
}

func absPath(p string) (string, error) {
	if p == "" {
		return "", status.Error(codes.InvalidArgument, "path is required")
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(defaultCwd, p)
	}
	return filepath.Clean(p), nil
}

// Exec streams a process's output. The main process runs in its own process
// group so a timeout kills everything it spawned.
func (s *Server) Exec(req *agentv1.ExecRequest, stream agentv1.Agent_ExecServer) error {
	if len(req.Argv) == 0 {
		return status.Error(codes.InvalidArgument, "argv is required")
	}
	start := time.Now()
	timeout := time.Duration(req.TimeoutMs) * time.Millisecond
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	maxOut := req.MaxOutputBytes
	if maxOut == 0 {
		maxOut = defaultMaxOutput
	}
	cwd := req.Cwd
	if cwd == "" {
		cwd = defaultCwd
	}
	os.MkdirAll(cwd, 0o755)

	env := mergeEnv(req.Env)
	path, err := lookPath(req.Argv[0], env)
	if err != nil {
		return sendExit(stream, &kilnv1.ExitStatus{Code: 127, Duration: durationpb.New(time.Since(start))},
			[]byte(err.Error()+"\n"))
	}

	outR, outW, err := os.Pipe()
	if err != nil {
		return status.Error(codes.Internal, err.Error())
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		return status.Error(codes.Internal, err.Error())
	}
	var stdin *os.File
	var stdinW *os.File
	if len(req.Stdin) > 0 {
		stdin, stdinW, err = os.Pipe()
	} else {
		stdin, err = os.Open(os.DevNull)
	}
	if err != nil {
		return status.Error(codes.Internal, err.Error())
	}

	var proc *os.Process
	exitCh, err := s.reaper.start(func() (int, error) {
		p, err := os.StartProcess(path, req.Argv, &os.ProcAttr{
			Dir:   cwd,
			Env:   flattenEnv(env),
			Files: []*os.File{stdin, outW, errW},
			Sys:   &unix.SysProcAttr{Setpgid: true},
		})
		if err != nil {
			return 0, err
		}
		proc = p
		return p.Pid, nil
	})
	// The child has its own copies; ours must close so EOF can arrive.
	outW.Close()
	errW.Close()
	stdin.Close()
	if err != nil {
		outR.Close()
		errR.Close()
		if stdinW != nil {
			stdinW.Close()
		}
		return sendExit(stream, &kilnv1.ExitStatus{Code: 126, Duration: durationpb.New(time.Since(start))},
			[]byte(err.Error()+"\n"))
	}
	pid := proc.Pid
	proc.Release()

	if stdinW != nil {
		go func() {
			stdinW.Write(req.Stdin)
			stdinW.Close()
		}()
	}

	var sendMu sync.Mutex
	send := func(ev *kilnv1.ExecEvent) error {
		sendMu.Lock()
		defer sendMu.Unlock()
		return stream.Send(ev)
	}
	var truncated sync.Map
	var wg sync.WaitGroup
	pump := func(r *os.File, mk func([]byte) *kilnv1.ExecEvent, name string) {
		defer wg.Done()
		var sent uint64
		buf := make([]byte, chunkSize)
		for {
			n, err := r.Read(buf)
			if n > 0 {
				if sent < maxOut {
					m := uint64(n)
					if sent+m > maxOut {
						m = maxOut - sent
						truncated.Store(name, true)
					}
					sent += m
					// Errors here mean the client went away; keep draining
					// so the child does not block on a full pipe.
					send(mk(append([]byte(nil), buf[:m]...)))
				} else {
					truncated.Store(name, true)
				}
			}
			if err != nil {
				return
			}
		}
	}
	wg.Add(2)
	go pump(outR, func(b []byte) *kilnv1.ExecEvent {
		return &kilnv1.ExecEvent{Event: &kilnv1.ExecEvent_Stdout{Stdout: b}}
	}, "stdout")
	go pump(errR, func(b []byte) *kilnv1.ExecEvent {
		return &kilnv1.ExecEvent{Event: &kilnv1.ExecEvent_Stderr{Stderr: b}}
	}, "stderr")

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	var ws unix.WaitStatus
	timedOut := false
	select {
	case ws = <-exitCh:
	case <-timer.C:
		timedOut = true
		unix.Kill(-pid, unix.SIGKILL)
		ws = <-exitCh
	case <-stream.Context().Done():
		unix.Kill(-pid, unix.SIGKILL)
		ws = <-exitCh
	}

	pumpsDone := make(chan struct{})
	go func() { wg.Wait(); close(pumpsDone) }()
	select {
	case <-pumpsDone:
	case <-time.After(drainGrace):
		// A background process inherited the pipes; stop waiting for EOF.
		outR.SetReadDeadline(time.Now())
		errR.SetReadDeadline(time.Now())
		<-pumpsDone
	}
	outR.Close()
	errR.Close()

	exit := &kilnv1.ExitStatus{
		Code:     int32(ws.ExitStatus()),
		TimedOut: timedOut,
		Duration: durationpb.New(time.Since(start)),
	}
	if ws.Signaled() {
		exit.Code = -1
		exit.Signal = unix.SignalName(ws.Signal())
	}
	if _, ok := truncated.Load("stdout"); ok {
		exit.OutputTruncated = true
	}
	if _, ok := truncated.Load("stderr"); ok {
		exit.OutputTruncated = true
	}
	return send(&kilnv1.ExecEvent{Event: &kilnv1.ExecEvent_Exit{Exit: exit}})
}

func sendExit(stream agentv1.Agent_ExecServer, exit *kilnv1.ExitStatus, stderr []byte) error {
	if err := stream.Send(&kilnv1.ExecEvent{Event: &kilnv1.ExecEvent_Stderr{Stderr: stderr}}); err != nil {
		return err
	}
	return stream.Send(&kilnv1.ExecEvent{Event: &kilnv1.ExecEvent_Exit{Exit: exit}})
}

func mergeEnv(extra map[string]string) map[string]string {
	env := make(map[string]string, len(baseEnv)+len(extra))
	for k, v := range baseEnv {
		env[k] = v
	}
	for k, v := range extra {
		env[k] = v
	}
	return env
}

func flattenEnv(env map[string]string) []string {
	out := make([]string, 0, len(env))
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	sort.Strings(out)
	return out
}

// lookPath resolves argv[0] against the request's PATH, not the agent's.
func lookPath(name string, env map[string]string) (string, error) {
	if filepath.IsAbs(name) || filepath.Base(name) != name {
		return name, nil
	}
	for _, dir := range filepath.SplitList(env["PATH"]) {
		p := filepath.Join(dir, name)
		if st, err := os.Stat(p); err == nil && !st.IsDir() && st.Mode()&0o111 != 0 {
			return p, nil
		}
	}
	return "", &exec.Error{Name: name, Err: exec.ErrNotFound}
}
