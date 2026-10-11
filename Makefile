TOOLS   := $(CURDIR)/.tools/bin
GOBIN   := $(CURDIR)/.tools/go/bin
export PATH := $(TOOLS):$(GOBIN):$(PATH)

BIN     := bin
GOFLAGS := -trimpath
LDFLAGS := -s -w

.PHONY: all tools proto build agent image test fmt vet clean tf-validate

all: build

# Pinned Firecracker, gVisor, protoc and Terraform into .tools/.
tools:
	./scripts/fetch-tools.sh

$(GOBIN)/protoc-gen-go:
	GOBIN=$(GOBIN) go install google.golang.org/protobuf/cmd/protoc-gen-go@v1.36.6
$(GOBIN)/protoc-gen-go-grpc:
	GOBIN=$(GOBIN) go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@v1.5.1

proto: $(GOBIN)/protoc-gen-go $(GOBIN)/protoc-gen-go-grpc
	protoc -I proto -I .tools/protoc/include \
	  --go_out=. --go_opt=module=github.com/tmjoris/kiln \
	  --go-grpc_out=. --go-grpc_opt=module=github.com/tmjoris/kiln \
	  proto/kiln/v1/kiln.proto proto/kiln/agent/v1/agent.proto

build:
	CGO_ENABLED=0 go build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o $(BIN)/ ./cmd/...

# The guest agent is static so it runs on any rootfs (musl or glibc).
agent:
	CGO_ENABLED=0 go build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o $(BIN)/kiln-agent ./cmd/kiln-agent

# Root filesystem + kernel for guests (no root required).
image: agent
	./images/build.sh

test:
	go test ./...

fmt:
	gofmt -s -w cmd internal
	terraform -chdir=deploy/terraform fmt -recursive

vet:
	go vet ./...

tf-validate:
	terraform -chdir=deploy/terraform init -backend=false -input=false >/dev/null
	terraform -chdir=deploy/terraform validate

clean:
	rm -rf $(BIN) build
