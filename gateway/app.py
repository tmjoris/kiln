"""REST gateway for kilnd.

A thin FastAPI layer over the kiln.v1.Kiln gRPC API for clients that speak
HTTP/JSON. Exec output can stream as server-sent events.

    KILN_ADDR=localhost:7070 uvicorn app:app --port 8080
"""

import base64
import json
import os
import sys
from contextlib import asynccontextmanager
from typing import Optional

import grpc
from fastapi import FastAPI, HTTPException, Request
from fastapi.responses import JSONResponse, StreamingResponse
from google.protobuf import duration_pb2
from pydantic import BaseModel, Field

sys.path.insert(0, os.path.join(os.path.dirname(__file__), "kiln_pb"))
from kiln.v1 import kiln_pb2 as pb  # noqa: E402
from kiln.v1 import kiln_pb2_grpc as pb_grpc  # noqa: E402

BACKENDS = {"": pb.BACKEND_UNSPECIFIED, "firecracker": pb.BACKEND_FIRECRACKER, "gvisor": pb.BACKEND_GVISOR}
GRPC_TO_HTTP = {
    grpc.StatusCode.NOT_FOUND: 404,
    grpc.StatusCode.INVALID_ARGUMENT: 400,
    grpc.StatusCode.RESOURCE_EXHAUSTED: 429,
    grpc.StatusCode.UNAVAILABLE: 503,
    grpc.StatusCode.DEADLINE_EXCEEDED: 504,
}

stub: pb_grpc.KilnStub


@asynccontextmanager
async def lifespan(_: FastAPI):
    global stub
    channel = grpc.aio.insecure_channel(
        os.environ.get("KILN_ADDR", "localhost:7070"),
        options=[("grpc.max_receive_message_length", 256 << 20)],
    )
    stub = pb_grpc.KilnStub(channel)
    yield
    await channel.close()


app = FastAPI(title="Kiln gateway", lifespan=lifespan)


@app.exception_handler(grpc.aio.AioRpcError)
async def grpc_error(_: Request, e: grpc.aio.AioRpcError):
    return JSONResponse({"detail": e.details()}, status_code=GRPC_TO_HTTP.get(e.code(), 502))


class CreateSandbox(BaseModel):
    backend: str = ""
    template: str = ""
    vcpus: int = 0
    memory_mib: int = 0
    idle_timeout_s: int = 0
    labels: dict[str, str] = {}


class Exec(BaseModel):
    argv: list[str] = Field(min_length=1)
    stdin: str = ""
    env: dict[str, str] = {}
    cwd: str = ""
    timeout_s: float = 60
    max_output_bytes: int = 0


class Run(BaseModel):
    sandbox: CreateSandbox = CreateSandbox()
    exec: Exec
    files: dict[str, str] = {}


def create_request(c: CreateSandbox) -> pb.CreateSandboxRequest:
    if c.backend not in BACKENDS:
        raise HTTPException(400, f"unknown backend {c.backend!r}")
    req = pb.CreateSandboxRequest(
        backend=BACKENDS[c.backend], template=c.template, labels=c.labels,
        resources=pb.Resources(vcpus=c.vcpus, memory_mib=c.memory_mib),
    )
    if c.idle_timeout_s:
        req.idle_timeout.FromSeconds(c.idle_timeout_s)
    return req


def exec_request(sandbox_id: str, e: Exec) -> pb.ExecRequest:
    timeout = duration_pb2.Duration()
    timeout.FromMilliseconds(int(e.timeout_s * 1000))
    return pb.ExecRequest(
        sandbox_id=sandbox_id, argv=e.argv, stdin=e.stdin.encode(), env=e.env,
        cwd=e.cwd, timeout=timeout, max_output_bytes=e.max_output_bytes,
    )


def sandbox_json(sb: pb.Sandbox) -> dict:
    return {
        "id": sb.id,
        "backend": pb.Backend.Name(sb.backend).removeprefix("BACKEND_").lower(),
        "template": sb.template,
        "vcpus": sb.resources.vcpus,
        "memory_mib": sb.resources.memory_mib,
        "start_mode": pb.StartMode.Name(sb.startup.mode).removeprefix("START_MODE_").lower(),
        "ready_ms": sb.startup.ready_latency.ToMilliseconds(),
        "created_at": sb.created_at.ToJsonString(),
        "labels": dict(sb.labels),
    }


def exit_json(ex: pb.ExitStatus) -> dict:
    return {
        "code": ex.code, "signal": ex.signal, "timed_out": ex.timed_out,
        "output_truncated": ex.output_truncated, "duration_ms": ex.duration.ToMilliseconds(),
    }


@app.get("/healthz")
async def healthz():
    return {"ok": True}


@app.post("/v1/sandboxes", status_code=201)
async def create(c: CreateSandbox):
    return sandbox_json(await stub.CreateSandbox(create_request(c)))


@app.get("/v1/sandboxes")
async def list_sandboxes():
    resp = await stub.ListSandboxes(pb.ListSandboxesRequest())
    return [sandbox_json(sb) for sb in resp.sandboxes]


@app.get("/v1/sandboxes/{sandbox_id}")
async def get(sandbox_id: str):
    return sandbox_json(await stub.GetSandbox(pb.GetSandboxRequest(id=sandbox_id)))


@app.delete("/v1/sandboxes/{sandbox_id}", status_code=204)
async def delete(sandbox_id: str):
    await stub.DeleteSandbox(pb.DeleteSandboxRequest(id=sandbox_id))


@app.post("/v1/sandboxes/{sandbox_id}/exec")
async def exec_(sandbox_id: str, e: Exec, request: Request):
    """Buffered JSON by default; `Accept: text/event-stream` streams events."""
    call = stub.Exec(exec_request(sandbox_id, e))
    if "text/event-stream" in request.headers.get("accept", ""):
        async def events():
            async for ev in call:
                kind = ev.WhichOneof("event")
                if kind == "exit":
                    data = exit_json(ev.exit)
                else:
                    data = getattr(ev, kind).decode(errors="replace")
                yield f"event: {kind}\ndata: {json.dumps(data)}\n\n"
        return StreamingResponse(events(), media_type="text/event-stream")
    stdout, stderr, exit_ = bytearray(), bytearray(), None
    async for ev in call:
        stdout += ev.stdout
        stderr += ev.stderr
        if ev.HasField("exit"):
            exit_ = exit_json(ev.exit)
    return {"stdout": stdout.decode(errors="replace"), "stderr": stderr.decode(errors="replace"), "exit": exit_}


@app.put("/v1/sandboxes/{sandbox_id}/files/{path:path}", status_code=204)
async def write_file(sandbox_id: str, path: str, request: Request, mode: Optional[int] = None):
    await stub.WriteFile(pb.WriteFileRequest(
        sandbox_id=sandbox_id, path="/" + path, content=await request.body(), mode=mode or 0))


@app.get("/v1/sandboxes/{sandbox_id}/files/{path:path}")
async def read_file(sandbox_id: str, path: str):
    resp = await stub.ReadFile(pb.ReadFileRequest(sandbox_id=sandbox_id, path="/" + path))
    return {"path": "/" + path, "content_b64": base64.b64encode(resp.content).decode()}


@app.post("/v1/run")
async def run(r: Run):
    resp = await stub.Run(pb.RunRequest(
        sandbox=create_request(r.sandbox),
        exec=exec_request("", r.exec),
        files={k: v.encode() for k, v in r.files.items()},
    ))
    return {
        "stdout": resp.stdout.decode(errors="replace"),
        "stderr": resp.stderr.decode(errors="replace"),
        "exit": exit_json(resp.exit),
        "start_mode": pb.StartMode.Name(resp.startup.mode).removeprefix("START_MODE_").lower(),
        "ready_ms": resp.startup.ready_latency.ToMilliseconds(),
    }


@app.get("/v1/stats")
async def stats():
    s = await stub.Stats(pb.StatsRequest())
    return {
        "active_sandboxes": s.active_sandboxes,
        "host_mem_available_bytes": s.host_mem_available_bytes,
        "pools": [{"key": p.key, "ready": p.ready, "target": p.target, "starting": p.starting} for p in s.pools],
    }
