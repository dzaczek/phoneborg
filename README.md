# PhoneBorg

[![CI](https://github.com/dzaczek/phoneborg/actions/workflows/ci.yml/badge.svg)](https://github.com/dzaczek/phoneborg/actions/workflows/ci.yml)

**Turn a drawer full of old Android phones into an AI inference cluster.**

Plug phones into a Linux or macOS host over USB. PhoneBorg installs a small
node agent on each phone over adb; the phone benchmarks itself, joins the
cluster and serves a GGUF model with
[llama.cpp](https://github.com/ggml-org/llama.cpp). Clients talk to **one
OpenAI-compatible endpoint**. The controller decides which phone serves which
model, routes each request to a phone, fails over when one freezes, and
exports everything to Prometheus and Grafana.

> **Status: proof of concept.** It runs end to end on a real Xiaomi Mi 8 and
> on emulated phones. APIs, metrics and file layout will change. Parts of the
> API are unauthenticated in v0: do not expose it to untrusted networks.

```text
 OpenAI client / opencode / curl          pbctl / web panel
            │  http://host:18080/v1              │  /admin (token)
            ▼                                    ▼
 ┌──────────────────────── host (runs adb) ─────────────────────────┐
 │  controller                                                      │
 │   ├─ registry     node inventory, heartbeats, lifecycle          │
 │   ├─ gateway      auth · affinity / pools · failover             │
 │   ├─ catalog +    model downloads, placement plan                │
 │   │  planner                                                     │
 │   └─ /metrics ──► Prometheus ──► Grafana                         │
 │  pcprov           adb provisioning, USB hot-plug watch           │
 └──────┬──────────────────────┬──────────────────────┬─────────────┘
        │ USB (adb reverse / adb forward)             │
   ┌────▼─────┐           ┌────▼─────┐           ┌────▼─────┐
   │ phone 1  │           │ phone 2  │    ...    │ phone N  │
   │ node-    │           │ node-    │           │ node-    │
   │ agent    │           │ agent    │           │ agent    │
   │  └ llama-│           │  └ llama-│           │  └ llama-│
   │   server │           │   server │           │   server │
   └──────────┘           └──────────┘           └──────────┘
```

## What it does

- **Zero-touch provisioning:** `pcprov watch` provisions phones as they are
  plugged in, picks the fastest llama.cpp build each CPU supports and
  restores dropped adb links.
- **Runtime-discovered inventory:** SoC, cores, RAM, battery, temperature;
  nothing hard-coded per phone model.
- **Node lifecycle:** `BENCHMARKING → ACTIVE → SUSPECT → OFFLINE` from
  heartbeats, with automatic recovery.
- **One OpenAI-compatible gateway:** chat and completions with streaming,
  session affinity for prompt-cache reuse, failover, thermal- and
  speed-aware routing.
- **Virtual models:** `auto`, `pool/<name>` and `node/<alias>` let each
  agent of a tool like opencode get its own phones.
- **Model catalog and placement:** add GGUFs from Hugging Face; phones
  download over USB and switch, sized to their RAM.
- **Performance tiers:** measured bandwidth per phone predicts each model's
  speed and keeps too-slow placements away.
- **Management:** `pbctl` CLI and a built-in web panel: drain, aliases,
  pools, API keys (stored hashed), gateway settings, usage per key and node.
- **External engine nodes:** add a Mac or PC running LM Studio, oMLX,
  Ollama or llama-server as a node, so a desktop model and the phones sit
  behind the same gateway.
- **OpenCode bridge:** `pbctl opencode` generates tool-less subagents that
  run on the cluster.
- **Observability:** JSON logs, `phoneborg_*` metrics, a Grafana dashboard.
- **Test without phones:** emulated Android phones (redroid) driven through
  real adb.

![PhoneBorg web panel: overview](docs/images/panel-overview.png)

More screenshots of the web panel and the Grafana dashboard: [docs/OPERATIONS.md](docs/OPERATIONS.md#screenshots).

## Quick start

Requirements: Go 1.25+, `adb`, and Docker (for the llama.cpp build,
monitoring and emulation).

**Real phones** (enable USB debugging first, see
[docs/REAL_PHONES.md](docs/REAL_PHONES.md)):

```sh
make test agent pcprov controller pbctl       # unit tests + binaries in bin/
make llama-all                                # static arm64 llama.cpp, all CPU variants (Docker)
make models/qwen2.5-0.5b-instruct-q4_k_m.gguf

bin/controller &                              # API + web panel on http://127.0.0.1:18080
bin/pcprov watch -model models/qwen2.5-0.5b-instruct-q4_k_m.gguf
# plug phones in and accept the USB debugging prompt, then:
curl http://127.0.0.1:18080/v1/chat/completions \
  -d '{"messages":[{"role":"user","content":"Hello from a phone cluster!"}]}'
```

**Emulated phones** with Prometheus (:9090) and Grafana (:3000), see
[docs/DEV_EMULATION.md](docs/DEV_EMULATION.md):

```sh
make agent pcprov cluster-up
make e2e                                      # provisioning, serving, failover, recovery
```

## Measured highlights

| What | Result |
|---|---|
| Qwen2.5-1.5B on a Xiaomi Mi 8 (Snapdragon 845) | 6.8 tok/s generation, best balance of 13 models tested |
| Qwen2.5-0.5B, Mi 8 vs emulated 4-core phone | ~12–15 vs 132 tok/s |
| Freezing a phone under load | 0 failed requests of 1138 |
| opencode, new session vs follow-up turn | 115 s vs 3 s (prompt cache via session affinity) |
| 3 parallel tool-less agents: `pool/fast` vs one phone | 9.9 s vs 23.8 s |
| Mi 8 screen off on USB power | 40 min `ACTIVE`, 40/40 requests served |

Setups and all other numbers: [docs/BENCHMARKS.md](docs/BENCHMARKS.md).

## Documentation

| Document | For |
|---|---|
| [docs/README.md](docs/README.md) | index by audience and task |
| [docs/REAL_PHONES.md](docs/REAL_PHONES.md) | choosing, preparing, provisioning and troubleshooting real phones |
| [docs/OPERATIONS.md](docs/OPERATIONS.md) | running the cluster: requests, web panel, `pbctl`, models, pools, keys, monitoring |
| [docs/DEV_EMULATION.md](docs/DEV_EMULATION.md) | developer environment with emulated phones, e2e and load tests |
| [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) | how it works: components, routing, placement, ports, security |
| [docs/BENCHMARKS.md](docs/BENCHMARKS.md) | every measurement, with setup and method |
| [docs/DECISIONS.md](docs/DECISIONS.md) | architecture decision records (ADR-001..016) |

## Repository layout

```text
controller/            control plane: registry, HTTP API, admin API, usage, metrics, placement
controller/gateway     OpenAI-compatible proxy: auth, targets, pickers, failover
controller/models      model catalog, GGUF metadata, planner, performance tiers
controller/ui          web panel (static files embedded in the controller)
controller/cmd/pbctl   admin CLI
node-agent/            on-phone agent: inventory, benchmark, heartbeats, runtime, model manager
provisioner/           pcprov: adb provisioning, hot-plug watch, heal, slim
internal/memplan       memory sizing shared by agent and planner
proto/                 controller <-> node wire types (JSON v0)
runtime/llama/         static arm64 llama.cpp build
deploy/                docker compose: controller, Prometheus, Grafana, emulated phones
tests/                 end-to-end test, LLM smoke test, load generator
docs/                  guides, architecture, benchmarks, ADRs
```

## License

[Apache License 2.0](LICENSE).
