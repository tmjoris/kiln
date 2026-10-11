// Package server exposes the sandbox manager as the kiln.v1.Kiln gRPC API.
package server

import (
	"context"
	"io"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	agentv1 "github.com/tmjoris/kiln/gen/kiln/agent/v1"
	kilnv1 "github.com/tmjoris/kiln/gen/kiln/v1"
	"github.com/tmjoris/kiln/internal/sandbox"
)

type Server struct {
	kilnv1.UnimplementedKilnServer
	m *sandbox.Manager
}

func New(m *sandbox.Manager) *Server { return &Server{m: m} }

func toProto(sb *sandbox.Sandbox) *kilnv1.Sandbox {
	return &kilnv1.Sandbox{
		Id:        sb.ID,
		Backend:   sb.Backend,
		Template:  sb.Spec.Template,
		Resources: &kilnv1.Resources{Vcpus: sb.Spec.VCPUs, MemoryMib: sb.Spec.MemMiB},
		State:     kilnv1.SandboxState_SANDBOX_STATE_READY,
		CreatedAt: timestamppb.New(sb.Created),
		Startup:   sb.Startup,
		Labels:    sb.Labels,
	}
}

func createOptions(req *kilnv1.CreateSandboxRequest) sandbox.CreateOptions {
	o := sandbox.CreateOptions{
		Backend:   req.GetBackend(),
		Spec:      sandbox.Spec{Template: req.GetTemplate()},
		Labels:    req.GetLabels(),
		ForceCold: req.GetForceColdBoot(),
		SkipPool:  req.GetSkipPool(),
	}
	if r := req.GetResources(); r != nil {
		o.Spec.VCPUs, o.Spec.MemMiB = r.Vcpus, r.MemoryMib
	}
	if d := req.GetIdleTimeout(); d != nil {
		o.IdleTimeout = d.AsDuration()
	}
	return o
}

func (s *Server) CreateSandbox(ctx context.Context, req *kilnv1.CreateSandboxRequest) (*kilnv1.Sandbox, error) {
	sb, err := s.m.Create(ctx, createOptions(req))
	if err != nil {
		return nil, err
	}
	return toProto(sb), nil
}

func (s *Server) GetSandbox(_ context.Context, req *kilnv1.GetSandboxRequest) (*kilnv1.Sandbox, error) {
	sb, err := s.m.Get(req.Id)
	if err != nil {
		return nil, err
	}
	return toProto(sb), nil
}

func (s *Server) ListSandboxes(context.Context, *kilnv1.ListSandboxesRequest) (*kilnv1.ListSandboxesResponse, error) {
	resp := &kilnv1.ListSandboxesResponse{}
	for _, sb := range s.m.List() {
		resp.Sandboxes = append(resp.Sandboxes, toProto(sb))
	}
	return resp, nil
}

func (s *Server) DeleteSandbox(_ context.Context, req *kilnv1.DeleteSandboxRequest) (*kilnv1.DeleteSandboxResponse, error) {
	return &kilnv1.DeleteSandboxResponse{}, s.m.Delete(req.Id)
}

func agentExec(req *kilnv1.ExecRequest) *agentv1.ExecRequest {
	return &agentv1.ExecRequest{
		Argv:           req.Argv,
		Stdin:          req.Stdin,
		Env:            req.Env,
		Cwd:            req.Cwd,
		TimeoutMs:      req.GetTimeout().AsDuration().Milliseconds(),
		MaxOutputBytes: req.MaxOutputBytes,
	}
}

// relay pumps one exec from the guest to fn and returns once the exit
// status has been delivered.
func relay(ctx context.Context, sb *sandbox.Sandbox, req *kilnv1.ExecRequest, fn func(*kilnv1.ExecEvent) error) error {
	if len(req.Argv) == 0 {
		return status.Error(codes.InvalidArgument, "argv is required")
	}
	st, err := sb.Guest().Exec(ctx, agentExec(req))
	if err != nil {
		return status.Errorf(codes.Unavailable, "sandbox agent: %v", err)
	}
	for {
		ev, err := st.Recv()
		if err == io.EOF {
			return status.Error(codes.Internal, "exec ended without exit status")
		}
		if err != nil {
			return err
		}
		if err := fn(ev); err != nil {
			return err
		}
		if ev.GetExit() != nil {
			return nil
		}
	}
}

func (s *Server) Exec(req *kilnv1.ExecRequest, stream kilnv1.Kiln_ExecServer) error {
	sb, err := s.m.Get(req.SandboxId)
	if err != nil {
		return err
	}
	return relay(stream.Context(), sb, req, stream.Send)
}

func (s *Server) WriteFile(ctx context.Context, req *kilnv1.WriteFileRequest) (*kilnv1.WriteFileResponse, error) {
	sb, err := s.m.Get(req.SandboxId)
	if err != nil {
		return nil, err
	}
	_, err = sb.Guest().WriteFile(ctx, &agentv1.WriteFileRequest{Path: req.Path, Content: req.Content, Mode: req.Mode})
	return &kilnv1.WriteFileResponse{}, err
}

func (s *Server) ReadFile(ctx context.Context, req *kilnv1.ReadFileRequest) (*kilnv1.ReadFileResponse, error) {
	sb, err := s.m.Get(req.SandboxId)
	if err != nil {
		return nil, err
	}
	r, err := sb.Guest().ReadFile(ctx, &agentv1.ReadFileRequest{Path: req.Path})
	if err != nil {
		return nil, err
	}
	return &kilnv1.ReadFileResponse{Content: r.Content}, nil
}

func (s *Server) Run(ctx context.Context, req *kilnv1.RunRequest) (*kilnv1.RunResponse, error) {
	if req.Exec == nil {
		return nil, status.Error(codes.InvalidArgument, "exec is required")
	}
	create := req.Sandbox
	if create == nil {
		create = &kilnv1.CreateSandboxRequest{}
	}
	sb, err := s.m.Create(ctx, createOptions(create))
	if err != nil {
		return nil, err
	}
	// Tear down off the request path; the caller only waits for output.
	defer func() { go s.m.Delete(sb.ID) }()
	for path, content := range req.Files {
		if _, err := sb.Guest().WriteFile(ctx, &agentv1.WriteFileRequest{Path: path, Content: content}); err != nil {
			return nil, err
		}
	}
	resp := &kilnv1.RunResponse{Startup: sb.Startup}
	err = relay(ctx, sb, req.Exec, func(ev *kilnv1.ExecEvent) error {
		resp.Stdout = append(resp.Stdout, ev.GetStdout()...)
		resp.Stderr = append(resp.Stderr, ev.GetStderr()...)
		if ex := ev.GetExit(); ex != nil {
			resp.Exit = ex
		}
		return nil
	})
	return resp, err
}

func (s *Server) Stats(context.Context, *kilnv1.StatsRequest) (*kilnv1.StatsResponse, error) {
	active, pools := s.m.Stats()
	avail, total, _ := sandbox.HostMemory()
	resp := &kilnv1.StatsResponse{
		ActiveSandboxes:       uint32(active),
		HostMemAvailableBytes: avail,
		HostMemTotalBytes:     total,
	}
	for _, p := range pools {
		resp.Pools = append(resp.Pools, &kilnv1.PoolStats{
			Key: p.Key, Ready: uint32(p.Ready), Target: uint32(p.Target), Starting: uint32(p.Starting),
		})
	}
	return resp, nil
}
