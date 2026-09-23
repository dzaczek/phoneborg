# PhoneBorg

**Turn a drawer full of old Android phones into an AI inference cluster.**

> **Status: proof of concept.** It works end to end on emulated phones and has
> run on a first real phone (Xiaomi Mi 8). APIs, metrics and file layout will
> change. Do not expose it to untrusted networks.

Plug phones into a Linux or macOS host over USB. PhoneBorg installs an agent on
each phone over ADB. The phone benchmarks itself, joins the cluster and serves
a GGUF model with [llama.cpp](https://github.com/ggml-org/llama.cpp). Clients
talk to **one OpenAI-compatible endpoint**. The controller routes each request
to a phone, retries it on another phone if one fails, and exports everything to
Prometheus and Grafana.

```text
 OpenAI client / opencode / curl
            │  http://host:18080/v1
            ▼
 ┌──────────────────────── host (runs adb) ─────────────────────────┐
 │  controller                                                      │
 │   ├─ registry     node inventory, heartbeats, state machine      │
 │   ├─ gateway      auth · session-affinity routing · failover     │
 │   └─ /metrics ──► Prometheus ──► Grafana                         │
 │                                                                  │
 │  pcprov           adb provisioning, USB hot-plug watch           │
 └──────┬──────────────────────┬──────────────────────┬─────────────┘
        │ USB (adb forward / adb reverse)             │
   ┌────▼─────┐           ┌────▼─────┐           ┌────▼─────┐
   │ phone 1  │           │ phone 2  │    ...    │ phone N  │
   │ node-    │           │ node-    │           │ node-    │
   │ agent    │           │ agent    │           │ agent    │
   │  └ llama-│           │  └ llama-│           │  └ llama-│
   │   server │           │   server │           │   server │
   └──────────┘           └──────────┘           └──────────┘
```

## What works today

- **Zero-touch provisioning.** `pcprov watch`: plug in a phone, accept the USB
  debugging prompt, and it joins. Unplug and replug re-provisions it.
- **Runtime-discovered inventory.** SoC, ABI, cores, RAM, storage, battery and
  temperature. Nothing is hard-coded per phone model.
- **Node lifecycle.** `BENCHMARKING → ACTIVE → SUSPECT → OFFLINE`, driven by
  heartbeats. Recovery is automatic.
- **LLM serving.** The agent supervises `llama-server` (restart with backoff,
  readiness checks). Models are pushed over USB.
- **OpenAI-compatible gateway.** `/v1/chat/completions`, `/v1/completions`,
  `/v1/models`, with streaming.
  - **Session affinity.** Requests that share a prompt prefix stay on the same
    phone, so the prompt cache is reused. With opencode, follow-up turns take
    3 s instead of ~2 min.
  - **Failover.** Failed attempts are retried on another phone. Requests stuck
    on a frozen phone are cancelled and re-routed.
  - **API keys** (optional), with per-key usage metrics. Keys are stored as
    SHA-256 hashes and can be created and revoked at runtime.
- **Cluster management.** A token-protected admin API (`/admin/`) and the
  `pbctl` CLI. Drain a phone for maintenance (running requests finish), remove
  nodes, manage API keys, and change the routing policy and timeouts without a
  restart. Usage per API key and per node (requests, errors, tokens, tok/s)
  can be persisted across restarts.
- **Observability.** Structured JSON logs, `phoneborg_*` Prometheus metrics and
  a provisioned Grafana dashboard: cluster health, latency, tokens/s, cache hit
  ratio, and token usage per API key (input/output).
- **Test without phones.** Emulated Android phones (redroid) in Docker, with
  per-phone RAM/CPU limits, driven through real adb.

## Quick start

Requirements: Go 1.25+, `adb`, and Docker for monitoring and emulation.

```sh
make test agent pcprov controller pbctl   # unit tests + binaries
make llama                            # static arm64 llama.cpp (built in Docker)
make models/qwen2.5-0.5b-instruct-q4_k_m.gguf

bin/controller &                      # API + dashboard on http://127.0.0.1:18080
bin/pcprov watch -model models/qwen2.5-0.5b-instruct-q4_k_m.gguf
# plug phones in (USB debugging enabled), then:
curl http://127.0.0.1:18080/v1/chat/completions \
  -d '{"messages":[{"role":"user","content":"Hello from a phone cluster!"}]}'
```

Full stack with Prometheus (:9090) and Grafana (:3000), plus two emulated
phones:

```sh
make cluster-up
make e2e                              # provisioning, serving, failover, recovery
```

Guides:
- [docs/USAGE.md](docs/USAGE.md): using the cluster: curl, OpenAI SDK and
  opencode, API keys with `pbctl`, draining phones, usage statistics, Grafana.
- [docs/REAL_PHONES.md](docs/REAL_PHONES.md): preparing phones, adb checks,
  provisioning, verification and troubleshooting.
- [docs/DEV_EMULATION.md](docs/DEV_EMULATION.md): emulator setup
  (macOS/colima), load testing, API keys and opencode.

## Measured

Qwen2.5-0.5B-Instruct Q4_K_M:

| | Xiaomi Mi 8 (real) | Emulated, 2 cores / 2 GB | Emulated, 4 cores / 3 GB |
|---|---|---|---|
| SoC | Snapdragon 845, LineageOS 22.2 | host (Apple M2) | host (Apple M2) |
| Generation | ~15 tok/s | 74 tok/s | 132 tok/s |
| Prompt processing | ~23–37 tok/s | 108 tok/s | 204 tok/s |

- Freezing a phone under load: 0 failed requests out of 1138.
- opencode, new session: 115 s. Follow-up turns: 3 s (99.9% prompt cache hits).

Emulated phones run on the host's CPU cores and are far faster than real
phones. See the limitations table in `docs/DEV_EMULATION.md`.

## Hardware target

Any ARM64 Android phone with USB debugging. The llama.cpp build targets
`armv8.2-a+dotprod` (Snapdragon 855 and newer). Older SoCs such as the
Snapdragon 845 lack dotprod and need `make llama ARM_ARCH=armv8.2-a+fp16`.

Preferred first-generation node: Snapdragon 865-class SoC, 8–12 GB RAM, USB-C.
Examples: OnePlus 8 / 8 Pro, Xiaomi Mi 10, Snapdragon Galaxy S20. Use a powered
USB hub.

Models: start with 0.5B–1.5B parameters, GGUF, Q4. A 2 GB phone has ~1.35 GB
free after Android itself.

## Repository layout

```text
controller/        control plane: registry, HTTP API, admin API, usage stats, metrics, dashboard
controller/gateway OpenAI-compatible proxy: auth, routing policies, failover
controller/cmd/pbctl  admin CLI: nodes, drain, API keys, stats, gateway settings
node-agent/        on-phone agent: inventory, benchmark, heartbeats, runtime supervisor
provisioner/       pcprov: adb provisioning and USB hot-plug watch
proto/             controller <-> node wire types (JSON v0, protobuf-ready)
runtime/llama/     static arm64 llama.cpp build
deploy/            docker compose: controller, Prometheus, Grafana, emulated phones
tests/             end-to-end tests, LLM smoke test, load generator
docs/              design decisions (ADRs), real-phone and emulation guides
```

## Roadmap

Done in this proof of concept:

- [x] Registration, inventory, benchmarks, heartbeats, metrics, dashboard
- [x] Single-phone LLM serving behind a cluster-wide gateway
- [x] Failover, session affinity, per-key usage metrics

Next:

- [ ] Validate on real phones: long runs with the screen off, thermals, vendor ROMs
- [ ] Android foreground service (Kotlin), survives reboots
- [ ] gRPC + mTLS transport; Wi-Fi nodes alongside USB
- [ ] Thermal-aware scheduling and llama-bench-based capability scores
- [ ] Distributed inference: split one model across phones

Design decisions and trade-offs: [docs/DECISIONS.md](docs/DECISIONS.md).

## License

[Apache License 2.0](LICENSE).
