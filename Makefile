.PHONY: all test agent pcprov controller pbctl cluster-up cluster-down e2e llama llama-all llm-smoke fmt lint clean help

VERSION ?= $(shell date +%Y%m%d%H%M)

# Kept in sync with .github/workflows/ci.yml.
STATICCHECK_VERSION ?= v0.7.0

all: test agent pcprov controller pbctl

test:
	go vet ./...
	go test ./...

# Static PIE for Android arm64, no NDK required.
agent:
	CGO_ENABLED=0 GOOS=android GOARCH=arm64 go build -trimpath \
		-ldflags "-s -w -X main.version=$(VERSION)" \
		-o bin/node-agent-android-arm64 ./node-agent/cmd/node-agent

pcprov:
	go build -o bin/pcprov ./provisioner/cmd/pcprov

controller:
	go build -o bin/controller ./controller/cmd/controller

# Admin CLI for the controller's /admin/ API.
pbctl:
	go build -o bin/pbctl ./controller/cmd/pbctl

cluster-up:
	sh deploy/colima-binder.sh
	docker compose -f deploy/docker-compose.yml up -d --build

cluster-down:
	docker compose -f deploy/docker-compose.yml down

e2e:
	bash tests/e2e/redroid_e2e.sh

# Static arm64 llama.cpp for phones (ADR-005 / ADR-007).
# Each variant is built into bin/llama/<ARM_ARCH>/, so pcprov can pick the
# right one per phone at provision time.
# Override: make llama LLAMA_TAG=bXXXX, or ARM_ARCH=armv8-a for SoCs without dotprod.
LLAMA_TAG ?= b11136
ARM_ARCH ?= armv8.2-a+dotprod+fp16
llama:
	docker build --build-arg LLAMA_TAG=$(LLAMA_TAG) --build-arg ARM_ARCH=$(ARM_ARCH) \
		-f runtime/llama/Dockerfile -o type=local,dest=bin/llama/$(ARM_ARCH) runtime/llama

# Builds every variant pcprov knows how to select between.
llama-all:
	$(MAKE) llama ARM_ARCH=armv8.2-a+dotprod+fp16
	$(MAKE) llama ARM_ARCH=armv8.2-a+fp16
	$(MAKE) llama ARM_ARCH=armv8-a

models/qwen2.5-0.5b-instruct-q4_k_m.gguf:
	mkdir -p models
	curl -fL -o $@ https://huggingface.co/Qwen/Qwen2.5-0.5B-Instruct-GGUF/resolve/main/qwen2.5-0.5b-instruct-q4_k_m.gguf

llm-smoke: models/qwen2.5-0.5b-instruct-q4_k_m.gguf
	bash tests/e2e/llm_smoke.sh

fmt:
	gofmt -w .

lint:
	@out=$$(gofmt -l .); if [ -n "$$out" ]; then echo "gofmt needs to be run on:"; echo "$$out"; exit 1; fi
	go vet ./...
	go run honnef.co/go/tools/cmd/staticcheck@$(STATICCHECK_VERSION) ./...

clean:
	rm -rf bin/

help:
	@echo "Targets:"
	@echo "  all          build test, agent, pcprov, controller, pbctl"
	@echo "  test         go vet and go test ./..."
	@echo "  agent        cross-compile node-agent for Android arm64"
	@echo "  pcprov       build the pcprov provisioner CLI"
	@echo "  controller   build the controller server"
	@echo "  pbctl        build the pbctl admin CLI"
	@echo "  cluster-up   load the binder module and start the dev cluster"
	@echo "  cluster-down stop the dev cluster"
	@echo "  e2e          run the redroid end-to-end test"
	@echo "  llama        build one llama.cpp variant (see ARM_ARCH/LLAMA_TAG)"
	@echo "  llama-all    build all llama.cpp ARM variants"
	@echo "  llm-smoke    download the smoke-test model and run tests/e2e/llm_smoke.sh"
	@echo "  fmt          format Go source with gofmt"
	@echo "  lint         gofmt check, go vet, and staticcheck"
	@echo "  clean        remove bin/ (never touches models/)"
	@echo "  help         list these targets"
