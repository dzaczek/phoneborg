.PHONY: all test agent pcprov controller cluster-up cluster-down e2e llama llm-smoke

VERSION ?= $(shell date +%Y%m%d%H%M)

all: test agent pcprov controller

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

cluster-up:
	sh deploy/colima-binder.sh
	docker compose -f deploy/docker-compose.yml up -d --build

cluster-down:
	docker compose -f deploy/docker-compose.yml down

e2e:
	bash tests/e2e/redroid_e2e.sh

# Static arm64 llama.cpp for phones (ADR-005).
# Override: make llama LLAMA_TAG=bXXXX, or ARM_ARCH=armv8-a for SoCs without dotprod.
LLAMA_TAG ?= b11136
ARM_ARCH ?= armv8.2-a+dotprod+fp16
llama:
	docker build --build-arg LLAMA_TAG=$(LLAMA_TAG) --build-arg ARM_ARCH=$(ARM_ARCH) \
		-f runtime/llama/Dockerfile -o type=local,dest=bin/llama runtime/llama

models/qwen2.5-0.5b-instruct-q4_k_m.gguf:
	mkdir -p models
	curl -fL -o $@ https://huggingface.co/Qwen/Qwen2.5-0.5B-Instruct-GGUF/resolve/main/qwen2.5-0.5b-instruct-q4_k_m.gguf

llm-smoke: models/qwen2.5-0.5b-instruct-q4_k_m.gguf
	bash tests/e2e/llm_smoke.sh
