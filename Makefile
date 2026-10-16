.PHONY: all test agent pcprov controller pbctl cluster-up cluster-down e2e llama llama-all llama-head llm-smoke fmt lint clean help

VERSION ?= $(shell date +%Y%m%d%H%M)

# Kept in sync with .github/workflows/ci.yml.
STATICCHECK_VERSION ?= v0.7.0
# staticcheck is built for the module's Go version; run it with that toolchain
# so a newer local Go (whose stdlib export data it cannot read) still works.
GO_MOD_VERSION := $(shell awk '/^go /{print $$2}' go.mod)

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
# The build runs on linux/arm64 also on an x86-64 host (via QEMU, see
# docs/INSTALL.md), so the binaries are always arm64.
LLAMA_TAG ?= b11136
ARM_ARCH ?= armv8.2-a+dotprod+fp16
llama:
	docker build --platform linux/arm64 --build-arg LLAMA_TAG=$(LLAMA_TAG) --build-arg ARM_ARCH=$(ARM_ARCH) \
		-f runtime/llama/Dockerfile -o type=local,dest=bin/llama/$(ARM_ARCH) runtime/llama

# Builds every variant pcprov knows how to select between.
llama-all:
	$(MAKE) llama ARM_ARCH=armv8.2-a+dotprod+fp16
	$(MAKE) llama ARM_ARCH=armv8.2-a+fp16
	$(MAKE) llama ARM_ARCH=armv8-a

# Big Brain head (ADR-034): llama-server with RPC, built natively for the
# controller host (needs cmake and a C++ compiler there). Same LLAMA_TAG as
# the phones: the RPC protocol must match.
llama-head:
	rm -rf bin/llama-head-src
	git clone --depth 1 --branch $(LLAMA_TAG) https://github.com/ggml-org/llama.cpp bin/llama-head-src
	cmake -S bin/llama-head-src -B bin/llama-head-src/build -DCMAKE_BUILD_TYPE=Release -DGGML_RPC=ON -DBUILD_SHARED_LIBS=OFF \
		-DGGML_NATIVE=ON -DLLAMA_CURL=OFF -DLLAMA_BUILD_TESTS=OFF
	cmake --build bin/llama-head-src/build -j 4 --target llama-server
	mkdir -p bin/llama-head && cp bin/llama-head-src/build/bin/llama-server bin/llama-head/ && rm -rf bin/llama-head-src

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
	GOTOOLCHAIN=go$(GO_MOD_VERSION) go run honnef.co/go/tools/cmd/staticcheck@$(STATICCHECK_VERSION) ./...

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
	@echo "  llama-head   build the Big Brain head (llama-server with RPC) for this host"
	@echo "  llm-smoke    download the smoke-test model and run tests/e2e/llm_smoke.sh"
	@echo "  fmt          format Go source with gofmt"
	@echo "  lint         gofmt check, go vet, and staticcheck"
	@echo "  clean        remove bin/ (never touches models/)"
	@echo "  help         list these targets"
