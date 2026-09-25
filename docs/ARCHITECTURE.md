# Architecture

How PhoneBorg works today. For *why* it works this way, follow the ADR links
into [DECISIONS.md](DECISIONS.md). For measurements, see
[BENCHMARKS.md](BENCHMARKS.md).

## Contents

- [Overview](#overview)
- [Components](#components)
- [Provisioning over adb](#provisioning-over-adb)
- [Node lifecycle](#node-lifecycle)
- [Request routing](#request-routing)
- [External engine nodes](#external-engine-nodes)
- [Model placement and switching](#model-placement-and-switching)
- [Performance tiers and resident bytes](#performance-tiers-and-resident-bytes)
- [Persistence](#persistence)
- [Ports](#ports)
- [Security model](#security-model)
- [Known limitations](#known-limitations)

## Overview

```text
 OpenAI client / opencode / curl        pbctl / web panel (/ui/)      Prometheus :9090 ─► Grafana :3000
            │ /v1/...                          │ /admin/... (token)          │ /metrics
            ▼                                  ▼                             ▼
 ┌─────────────────────────── host (runs adb) ──────────────────────────────────────────┐
 │ controller :18080                                                                    │
 │   registry ── node inventory, heartbeats, lifecycle, drain flags, aliases            │
 │   gateway ─── auth · target resolution · pickers · failover · reaping · usage        │
 │   catalog ─── GGUF downloads + metadata        planner ── which node serves what     │
 │   admin API · web panel · usage store · /metrics · /status                           │
 │                                                                                      │
 │ pcprov ───── adb provisioning, hot-plug watch, adb link heal, slim                   │
 └──────┬────────────────────────────────────────────────────────────┬──────────────────┘
        │ USB: adb reverse tcp:18080 (phone ─► controller)            │
        │      adb forward tcp:<host port> ─► tcp:18090 (controller ─► llama-server)
   ┌────▼──────────────────────────┐                           ┌──────▼─────┐
   │ phone: /data/local/tmp/phoneborg                          │  phone N   │
   │  node-agent (shell user)      │                           │    ...     │
   │   inventory · benchmark ·     │                           └────────────┘
   │   heartbeats · model manager  │
   │   └─ llama-server 127.0.0.1:18090                         
   └───────────────────────────────┘
```

Everything the phones do goes over the USB cable: the agent talks to
`127.0.0.1:18080` on the phone, which `adb reverse` maps to the controller,
and the controller reaches each phone's llama-server through an `adb forward`
on the host (ADR-003, ADR-006).

## Components

### Controller (`controller/`, binary `controller`)

| Part | Code | Role |
|---|---|---|
| Registry | `controller/registry.go` | Nodes by id, inventory, last heartbeat, lifecycle state, drain flags and aliases (kept by node id, so they survive re-registration). |
| Gateway | `controller/gateway` | OpenAI-compatible `/v1/chat/completions`, `/v1/completions`, `/v1/models`. Authenticator, target resolution, `Picker`s (`Affinity`, `LeastInflight`, `Spread`), failover, reaping, token and usage accounting, prewarm. |
| Admin API | `controller/admin.go`, `models_admin.go`, `pools.go`, `external.go` | `/admin/...` JSON API behind a bearer token: nodes, drain, keys, stats, gateway settings, models, placement, pools, aliases, prewarm, external nodes (ADR-008). |
| External nodes | `controller/external.go` | Operator-added OpenAI-compatible servers (LM Studio, oMLX, Ollama, llama-server on a Mac or PC): health and model polling, self-tests, backends for the gateway (ADR-016). |
| Catalog | `controller/models` | Downloads GGUF files (`hf://`, `https://`, `file://`), resumes, hashes, reads GGUF metadata, computes RAM estimate and `resident_bytes` (ADR-011, ADR-012 update). |
| Planner | `controller/models/planner.go` | Pure function from policies, default model, nodes and ready models to one model per node (ADR-011, ADR-015). |
| Performance | `controller/perf.go`, `controller/models/perf.go` | Per-node effective bandwidth, performance tier, predicted tok/s (ADR-015). |
| Usage store | `controller/usage.go` | Requests, errors and tokens per API key and per node, since start and, with `-state-dir`, since first use. |
| Web panel | `controller/ui` | Static HTML/JS embedded in the binary, served at `/ui/`; an admin API client like `pbctl` (ADR-013). |
| Metrics | `/metrics` | `phoneborg_*` Prometheus metrics. |

`pbctl` (`controller/cmd/pbctl`) is the admin CLI. It imports the admin API's
types from `controller`, so the two cannot drift apart.

### Node agent (`node-agent/`, binary `node-agent`)

A static Go binary pushed to `/data/local/tmp/phoneborg` and started over adb
as the `shell` user (ADR-001).

| Part | Code | Role |
|---|---|---|
| Inventory | `inventory.go` | SoC, ABI, cores, RAM, storage, battery, temperature from `getprop`, `/proc`, `/sys`, `dumpsys`, capped by cgroup limits (so emulated phones report their container limits). Every probe degrades to "unknown". |
| Benchmark | `bench.go` | A short synthetic CPU/memory benchmark (`synthetic-go-v0`) at startup, before llama-server starts. Only a fallback for routing. |
| Runtime supervisor | `runtime.go` | Runs llama-server, restarts it with backoff, probes `/health`, runs the self-test after every (re)start (ADR-010). |
| Model manager | `manager.go`, `download.go` | Applies the controller's desired model: download, verify, size, load, fall back (ADR-012). |
| Memory sizing | `sizing.go`, `internal/memplan` | Picks context, slots and KV cache type that fit the RAM budget; shared with the planner. |
| Thread choice | `cpu.go` | `-threads-policy all` (default) or `big`, capped by usable cores (ADR-009). |
| Self-test | `runtime.go` | Fixed ~100-token prompt, 32 generated tokens, no prompt cache; reports prompt and generation tok/s. |

### Provisioner (`provisioner/`, binary `pcprov`)

`pcprov` checks the ABI, reads `/proc/cpuinfo` and picks the fastest
llama.cpp build the CPU supports (`bin/llama/<ARM_ARCH>/`, ADR-007), pushes
the agent, llama-server and an optional model, sets up the adb links and
starts the agent detached. `watch` does this for every device that appears
(polling every 3 s) and re-checks adb links every 15 s. `slim`/`unslim`
reversibly disable user-facing apps to free RAM.

### Monitoring (`deploy/`)

`make cluster-up` starts the controller, Prometheus (scrapes
`controller:18080/metrics` every 5 s; retention 15 days or 1 GB) and Grafana
with the provisioned "PhoneBorg" dashboard, plus two emulated phones.

## Provisioning over adb

```text
 pcprov provision / watch                                    phone
 ───────────────────────                                     ─────
 1. adb devices, (adb connect host:port for -connect)
 2. getprop ro.product.cpu.abilist  ── must be arm64-v8a
 3. /proc/cpuinfo Features ─► pick llama.cpp variant (dotprod+fp16 │ fp16 │ armv8-a)
 4. adb reverse tcp:18080 tcp:18080          phone 127.0.0.1:18080 ─► controller
 5. adb push node-agent, bin/llama-server,  ─► /data/local/tmp/phoneborg/
    models/<file>.gguf (skipped if same size)
 6. adb forward tcp:0 tcp:18090  ─► host port P   controller ─► P ─► llama-server
    (an existing forward is reused, so P stays stable)
 7. start node-agent detached  (-advertise-port P, -runtime-variant, -model, -agent-args)
 8. optional: slim

 watch loop: every 3 s list devices; new or re-appeared device ─► steps 1–8
             every 15 s per device: heal = re-add missing reverse / forward
```

`adb` drops forward and reverse rules when a phone re-enumerates on USB (a
loose cable, a hot phone) while it stays listed. `pcprov watch` restores them
on the same host port the agent advertises; `pcprov heal -serial S` does it
once.

## Node lifecycle

```text
                register                   benchmark report
   (new) ───────────────► BENCHMARKING ───────────────────► ACTIVE
                                ▲                          │   ▲
                   heartbeat    │          no heartbeat    │   │ heartbeat
                   resumed,     │          for 3 intervals │   │ resumed
                   no benchmark │                          ▼   │
                                └──────────────────────── SUSPECT
                                                           │
                                          no heartbeat     │
                                          for 6 intervals  ▼
                                                        OFFLINE ── heartbeat resumed ─► ACTIVE
```

- The controller asks for heartbeats every 5 s (`-heartbeat-interval`), so
  `SUSPECT` comes after 15 s and `OFFLINE` after 30 s by default
  (`-suspect-after-missed 3`, `-offline-after-missed 6`).
- A heartbeat from an id the controller does not know (e.g. after a
  controller restart) gets an error, and the agent registers again. After
  three failed heartbeats in a row the agent also starts over, with backoff
  up to 30 s.
- **Drain** is a flag, not a state: a drained `ACTIVE` node gets no new
  requests, but running ones finish. **Hot** (temperature ≥ thermal limit)
  and **switching** (downloading or loading a model) are also per-node
  conditions checked at routing time.
- `pbctl forget` removes a node; if it is still running it registers again
  on its next heartbeat.

## Request routing

```text
 request ─► authenticate (open: anonymous │ keys: 401 without a valid key)
        ─► resolve "model":
             <model id>        ready nodes serving it
             auto              any ready node
             pool/<name>       eligible pool members  (pool picker: spread │ affinity)
             node/<alias|id>   exactly that node      (no picker, no failover; 503 node_unavailable)
                               (an external node's name, or ext:<name>, too)
             unknown pool/node 404 model_not_found
        ─► routable = ACTIVE + runtime ready + not drained + not switching
                      (+ ACTIVE external nodes, one backend per model)
        ─► skip nodes at their max_concurrency (external nodes); none left ─► 503 busy
        ─► context filter: estimated tokens (body bytes / 4) > every node's context
                            ─► 400 context_length_exceeded; smaller nodes are skipped
        ─► picker chooses a node
             Affinity (default): same prompt prefix (first message + tools) ─► same node;
                                 skip a pinned node that is ≥ spill (2) busier or hot;
                                 new large sessions (≥ 16 KiB) ─► least busy, fewest
                                 large sessions, fastest (self-test tok/s)
             LeastInflight:      fewest in-flight requests
             Spread (pools):     fewest in-flight, then fastest, then rotation; no pins
             hot nodes get no new sessions unless every candidate is hot
        ─► forward over adb forward (timeout -upstream-timeout, default 120 s)
             refused / 5xx before any byte sent ─► retry once on another node,
                                                    avoid the failed node for 30 s
             all attempts failed ─► 502 backends_failed
```

- **Reaping.** Every second the controller sweeps node states and cancels
  in-flight requests on nodes that left the ready set (e.g. `SUSPECT`); those
  requests are retried elsewhere instead of waiting for the timeout. Drained
  nodes are not reaped.
- A node that rejects a prompt as too long for its context is not marked down;
  the request just tries another node.
- Once response bytes have reached the client, a request cannot be retried.
- `X-PhoneBorg-Node` on the response names the node that answered.
- Rationale: ADR-006 (gateway, affinity, failover), ADR-010 (measured speed,
  thermal), ADR-014 (targets, pools, spread).

## External engine nodes

An operator can add any OpenAI-compatible server as a node with
`PUT /admin/external/<name>` (`pbctl external add`). The gateway sees it as
node id `ext:<name>` with alias `<name>`; it has no agent, heartbeats,
inventory or RAM class, and the planner never assigns it a model (ADR-016).

```text
 controller ── every 10 s: GET {url}/v1/models (bearer api_key, 5 s timeout)
      │          ok ─► ACTIVE, models = listed ids ∩ allowlist
      │          3 failures in a row ─► OFFLINE (last_error kept)
      │        on registration / model-list change: self-test each new model
      │          (fixed prompt, max_tokens 32) ─► gen tok/s = routing Speed
      ▼
 gateway ── one Backend per (external node, model), URL = {url}
            body "model" rewritten to the real model id
            Authorization: Bearer <api_key> (client headers are never forwarded)
            at most max_concurrency requests in flight (default 1)
```

External nodes take part in `auto`, pools (node filters match `<name>` or
`ext:<name>`; class filters never match them), `node/<name>`, plain model
ids, affinity, failover, reaping when they turn `OFFLINE`, drain, usage and
every per-node gateway metric. `node/<name>` goes to the node's first model
(allowlist order, else the order the server lists). They are not in
`/admin/nodes`, `/admin/placement` or the device class counts.

## Model placement and switching

```text
 pbctl models add hf://…  ─► catalog: download (resume, 3 retries) ─► SHA-256 ─► parse GGUF
                                      ─► size, resident_bytes, layers, KV heads ─► ready
 pbctl placement set …    ─► policies (pin │ replicas │ percent, classes, min_tok_s) + default model
                                      │
 planner (on every change, node join/leave/drain, model ready, and every 10 s)
   pins ─► replicas ─► percent ─► default model ─► otherwise keep what the node serves
   eligible = fits (node budget, or class heuristic before the first heartbeat)
              and predicted tok/s ≥ min_tok_s (unknown never excludes; pins always place)
                                      │
 heartbeat reply: 200 {desired: model id, URL, SHA-256, size, resident bytes,
                       ctx 16384 (≤ trained), slots 1, kv auto, GGUF shape}   (204 = no change)
                                      │
 node agent (one switch at a time, latest desired state wins)
   cached with right SHA-256? ─ no ─► statfs; evict other .gguf LRU (never the served one)
                                      ─► GET /v1/model-files/<id> over adb reverse
                                         (models/<id>.gguf.part, HTTP Range resume)
                                      ─► verify SHA-256 ─► rename
   size: budget = MemAvailable + RssAnon(llama-server) − 600 MiB (-mem-reserve-mb)
         try f16 KV ─► q8_0 KV ─► halve ctx to 4096 ─► 1 slot ─► fail
   load: stop old llama-server ─► start new one ─► /health (2 min) ─► self-test
   state: downloading ─► loading ─► serving       (the gateway skips switching nodes)
   on failure: keep or restart the previous model; state error; retry the same
               desired state after 30 s, doubling to 10 min (sooner if budget grows >10%)
```

A node provisioned with `pcprov -model FILE` serves that file until the
controller sends a desired model. If the requested settings do not fit but
the same model is already running, the agent keeps the running instance
instead of failing. Rationale: ADR-011 (catalog, planner, delivery) and
ADR-012 (agent side).

## Performance tiers and resident bytes

- **RAM class** (`xs` < 3 GiB, `s` 3–5, `m` 5–7, `l` 7–10, `xl` 10+) says
  whether a model can fit.
- **Performance tier** says whether it runs fast enough. The controller
  computes `gen_gbps = self-test gen tok/s × resident bytes of the served
  model / 1e9` on every heartbeat, keeps the last good value per node (also
  across restarts, in `routing.json`) and maps it to `t1` < 4, `t2` 4–10,
  `t3` 10–25, `t4` ≥ 25 GB/s, or `?` before the first self-test.
- **Predicted speed** of a candidate model on a node is
  `gen_gbps × 1e9 / resident_bytes`. The planner will not place a model
  where the prediction is below the policy's `min_tok_s` or
  `-min-predicted-tok-s` (default 3).
- **Resident bytes** are the file size minus tensors llama.cpp reads only a
  few rows of through mmap (today: `per_layer_token_embd.weight`, as in
  Gemma 3n). Sizing adds 10% of those sparse bytes back. The same number is
  used for fit checks, agent sizing, bandwidth and prediction.

Rationale: ADR-011, ADR-012 update, ADR-015. Numbers: [BENCHMARKS.md](BENCHMARKS.md#effective-bandwidth-and-performance-tiers).

## Persistence

With `-state-dir DIR` the controller keeps:

| File | Contents | Written |
|---|---|---|
| `DIR/usage.json` | usage totals per key and node since first use | every 30 s and on SIGINT/SIGTERM; a corrupt file stops startup |
| `DIR/models.json` | model catalog | on change |
| `DIR/placement.json` | placement policies and the default model | on change |
| `DIR/routing.json` | node aliases, pools, per-node measured bandwidth | on change (atomic, mode 0600) |
| `DIR/external.json` | external nodes with their API keys and self-test results | on change (atomic, mode 0600) |
| `DIR/models/` | downloaded GGUF files (`-models-dir` overrides) | by the catalog |

Elsewhere:

| What | Where |
|---|---|
| API keys (hashed) | the `-api-keys-file` file, if given; otherwise memory only |
| Admin token | the `-admin-token-file` file (only its SHA-256 is kept in memory) |
| Not persisted | drain flags (phones and external nodes), runtime gateway settings (policy, spill, timeout, thermal limit, auth mode) |
| On each phone | `/data/local/tmp/phoneborg`: `node-agent`, `bin/llama-server`, `models/*.gguf`, `agent.log`/`agent.pid`, `runtime.log`/`runtime.pid`, `slim.state` |

Without `-state-dir` everything lives in memory and model files go to a
temporary directory. The compose stack uses `-state-dir /var/lib/phoneborg`
on the `controller-state` volume.

## Ports

| Port | Where | What |
|---|---|---|
| 18080 | host | controller: gateway `/v1`, admin `/admin`, `/ui/`, `/status`, `/metrics`, `/healthz` (`-listen`) |
| 18080 | phone | `adb reverse` to the controller (`pcprov -controller-port`) |
| 18090 | phone, 127.0.0.1 only | llama-server (`pcprov -serve-port`, agent `-serve-port`) |
| ephemeral | host | `adb forward` to each phone's 18090, chosen by adb and reused on re-provisioning; the controller reaches it on `-backend-host` (default 127.0.0.1, `host.docker.internal` in compose) |
| 18091 | phone | `tests/e2e/llm_smoke.sh` test server, so it does not disturb the agent |
| 9090 | host | Prometheus (compose) |
| 3000 | host | Grafana (compose) |
| 5555, 5556 | host | adb of the emulated phones `phone-low`, `phone-mid` (compose) |

## Security model

PhoneBorg is a proof of concept for localhost or a trusted network. There is
no TLS anywhere yet.

| Surface | Protection |
|---|---|
| Gateway `/v1/chat/completions`, `/v1/completions`, `/v1/models` | open (served as `anonymous`) without `-api-keys-file`; with it, `Authorization: Bearer <key>` or `x-api-key` is required. Keys are stored as SHA-256 hashes; plaintext is shown once at creation (ADR-008). |
| Admin API `/admin/...` | disabled (503) unless `-admin-token-file` is set; then a bearer token of ≥ 16 characters, compared by hash in constant time. Every call is counted in `phoneborg_admin_actions_total`, every change logged. |
| Web panel `/ui/` | static files, no data; it uses the admin token (kept in the tab's `sessionStorage`). Strict CSP, no external resources (ADR-013). |
| Node protocol `/v1/register`, `/v1/benchmark`, `/v1/heartbeat` | **unauthenticated** in v0: phones reach it only through `adb reverse` on the adb host (ADR-002, ADR-003). |
| `/v1/nodes`, `/status`, `/metrics`, `/healthz` | **unauthenticated**, read-only |
| `/v1/model-files/<id>` | **unauthenticated**, same trust as the heartbeat: anything that reaches the port can download catalog models, including `file://` sources (ADR-011). |
| llama-server on the phone | bound to the phone's 127.0.0.1, reachable only through `adb forward` |
| External nodes (ADR-016) | the controller sends each its own API key, if set; the key is stored in `external.json` (mode 0600), never returned by the admin API and never logged. A client's PhoneBorg key is never forwarded. The external server's own exposure is the operator's responsibility. |

Why unauthenticated in v0: the node protocol is meant to move to gRPC + mTLS
before any non-localhost deployment (ADR-002). Until then, keep port 18080 on
localhost or a trusted network and do not expose it to untrusted networks.
`deploy/dev-admin-token` is public and for development only.

## Known limitations

- Phones must stay on USB to the adb host; no Wi-Fi nodes yet (ADR-003).
- The agent is started over adb and does not survive a phone reboot;
  `pcprov watch` re-provisions when the phone is plugged in (ADR-001).
- One model per phone; no model split across phones.
- CPU only (static musl llama.cpp, no GPU/NPU backends, ADR-005).
- One admin role, no per-operator identity; keys have no scopes, expiry or
  quotas (ADR-008).
- Drain flags and runtime gateway settings are lost on controller restart.
- Hugging Face gated repositories and split GGUF files are not supported
  (ADR-011).
- Thermal routing looks at the last heartbeat only (a soft limit, ADR-010).
- Performance prediction is a single bandwidth number per node; it ignores
  attention variants and MoE (ADR-015).
- External nodes are not managed: no placement, no downloads, no thermal
  data. Requests beyond an external node's `max_concurrency` are not queued;
  they go elsewhere or get 503 `busy` (ADR-016).
