# Architecture Decision Records

Why PhoneBorg is built the way it is. The current system is described in
[ARCHITECTURE.md](ARCHITECTURE.md) and all measurements are collected in
[BENCHMARKS.md](BENCHMARKS.md). ADRs are kept as written; later changes are
added as dated "Update (2026-09)" notes instead of rewriting the original
text.

| ADR | Title | Status | Summary |
|---|---|---|---|
| [001](#adr-001-milestone-1-node-agent-is-a-go-binary-launched-over-adb-not-an-android-app) | Node agent is a Go binary launched over adb | accepted | Static binary in `/data/local/tmp`, no Android app yet; does not survive reboots. |
| [002](#adr-002-v0-transport-is-json-over-http) | v0 transport is JSON over HTTP | accepted | Curl-able wire types in `proto/v0.go`; gRPC + mTLS before non-localhost use. |
| [003](#adr-003-phones-reach-the-controller-through-adb-reverse) | Phones reach the controller through `adb reverse` | accepted | Agent always talks to `127.0.0.1:18080`; phones stay on USB. |
| [004](#adr-004-redroid-as-the-development-phone) | redroid as the development "phone" | accepted | Real arm64 Android in containers with real adb, sized by cgroups. |
| [005](#adr-005-llamacpp-as-a-static-musl-arm64-binary-no-ndk-for-first-llm-tests) | Static musl llama.cpp, no NDK | superseded in part by 007 | CPU-only static build; variant selection moved to ADR-007. |
| [006](#adr-006-inference-gateway-in-the-controller-reaching-phones-over-adb-forward) | Gateway in the controller, phones over `adb forward` | accepted, updated by 010 and 014 | One OpenAI endpoint; affinity picker, failover, reaping. |
| [007](#adr-007-pcprov-selects-the-llamacpp-build-from-the-phones-cpu-features) | pcprov selects the llama.cpp build per CPU | accepted | `make llama-all` builds three variants; pcprov picks the fastest that runs. |
| [008](#adr-008-admin-api-and-usage-statistics) | Admin API and usage statistics | accepted | Token-protected `/admin/`, `pbctl`, hashed API keys, persisted usage. |
| [009](#adr-009-llama-server-threads-on-biglittle-phones) | llama-server threads on big.LITTLE | accepted | Default `-threads-policy all`; benchmark through llama-server. |
| [010](#adr-010-measured-inference-speed-and-thermal-aware-routing) | Measured speed and thermal-aware routing | accepted | Self-test tok/s replaces the synthetic score; hot nodes get no new sessions. |
| [011](#adr-011-model-catalog-and-placement) | Model catalog and placement | accepted, updated by 012 and 015 | Controller downloads GGUFs, plans placement, phones download over USB. |
| [012](#adr-012-agent-side-model-switching-and-memory-sizing) | Agent-side model switching and memory sizing | accepted, with updates | Download, verify, size to RAM, load, fall back; resident bytes. |
| [013](#adr-013-web-management-panel) | Web management panel | accepted | Static embedded panel that is just another admin API client. |
| [014](#adr-014-virtual-models-and-pools-for-agent-workloads) | Virtual models and pools | accepted | `auto`, `pool/<name>`, `node/<alias>`; `spread` routing for small agents. |
| [015](#adr-015-performance-tiers-from-measured-bandwidth) | Performance tiers from measured bandwidth | accepted, with update | Effective bandwidth per node, tiers `t1`..`t4`, predicted tok/s gate placement. |
| [016](#adr-016-external-engine-nodes) | External engine nodes | accepted | Operator-added OpenAI-compatible servers (`ext:<name>`) routed like phones, polled via `/v1/models`, never planned. |
| [017](#adr-017-ollama-compatible-front-and-local-by-default-access) | Ollama-compatible front and local-by-default access | accepted | `/api/*` translates to the OpenAI gateway; `-gateway-access local\|keys\|open` gates inference/listing endpoints by peer CIDR. |
| [018](#adr-018-chat-test-in-the-web-panel) | "Chat test" in the web panel | accepted | Admin-token-authenticated `POST /admin/chat/completions` proxies through the normal gateway path, attributed to principal `panel`. |
| [019](#adr-019-usb-device-auto-detection-in-the-panel-and-conflict-free-provisioning) | USB device auto-detection in the panel and conflict-free provisioning | accepted | `pcprov watch` reports its adb device view to a new admin endpoint; `auto_provision` setting and one-shot provision/retry; per-serial backoff, duplicate/placeholder rejection and a host-wide flock. |
| [020](#adr-020-super-borg-mode--one-model-an-orchestrator-delegating-to-phones) | Super Borg mode | accepted, replaced in part by 022 | A cluster-wide mode in which an orchestrator node delegates subtasks to the other nodes through a `delegate` tool; idle timeout for orchestrator calls. |
| [021](#adr-021-super-borg-jobs--long-work-with-server-side-state) | Super Borg jobs | accepted | Long work in the background: task list and documents on the controller, stateless one-tool orchestrator steps, guards against small-model mistakes, gateway-assigned parallel workers. |
| [022](#adr-022-super-borg-as-a-pool-routing-instead-of-a-cluster-wide-mode) | Super Borg as a pool routing | accepted | `routing: superborg` on pools replaces the cluster-wide mode; the old setting migrates to `pool/superborg`; jobs run on a pool. |
| [023](#adr-023-route-tool-requests-to-tool-capable-models) | Route tool requests to tool-capable models | accepted | Catalog tag `tools`; requests with tools avoid untagged models; the orchestrator choice prefers them. |
| [024](#adr-024-separate-limits-for-prompt-processing-and-generation) | Separate limits for prompt processing and generation | accepted | `-first-token-timeout` until the first byte, then `-upstream-timeout` as an idle limit. |
| [025](#adr-025-the-cluster-as-mcp-tools-pbctl-mcp) | The cluster as MCP tools | accepted | `pbctl mcp` over stdio: ask, map, vote, jobs; opencode config, agents and instructions generated by `pbctl opencode init`. |
| [026](#adr-026-named-admin-tokens) | Named admin tokens | accepted | Hashed per-device admin tokens next to `-admin-token-file`; the audit log names the token. |
| [027](#adr-027-pool-status-onoff-switch-and-no-conflicting-pools) | Pool status, on/off switch, no conflicting pools | accepted | Computed pool status, `disabled` flag, 409 for enabled pools that claim one node with no common model. |
| [028](#adr-028-multi-model-benchmark-mmb) | Multi-model benchmark (MMB) | accepted | Load every fitting model on chosen phones under a benchmark override and time cold, warm and short requests. |
| [029](#adr-029-external-orchestrators) | External orchestrators | accepted | External nodes with `role=orchestrator` get no normal traffic and orchestrate only the Super Borg pools that name them. |
| [030](#adr-030-firmware-check) | Firmware check | accepted | Agents report build, patch and bootloader; with `-firmware-check` the controller compares them daily with LineageOS, Google's OTA page and the Xiaomi tracker. |
| [031](#adr-031-grafana-in-the-panel) | Grafana in the panel | accepted | The controller serves Grafana under `/grafana/`; the panel embeds the dashboard and per-node charts with a cookie-based session. |
| [032](#adr-032-token-analyzer) | Token analyzer | accepted | The phones' own tokenizers count a text per served model; Grafana dashboards break token usage down per phone, model and key, and show one phone at a time. |
| [033](#adr-033-semantic-router-for-auto-experimental) | Semantic router for "auto" | accepted, experimental | One phone sorts each `auto` request into operator-editable classes from a one-token logprobs answer; each class has a target; any failure is plain `auto`. |

## ADR-001: Milestone-1 node agent is a Go binary launched over ADB, not an Android app

**Problem.** The target architecture is a Kotlin app + NDK runtime on the phone. Milestone 1
only needs inventory, benchmark and heartbeats, and must work with zero taps on
the phone beyond accepting USB debugging.

**Alternatives.**
1. Kotlin app with a foreground service (installed via `adb install`).
2. Static Go binary pushed to `/data/local/tmp` and run as the `shell` user.

**Trade-offs.** (2) needs no Gradle/SDK/signing and runs identically on any
arm64 Android 7+ phone. But the process is not managed by Android: it does not
restart after reboot, and some vendor ROMs may kill it. The `shell` user also
cannot read some sensors (SELinux), so probes must degrade gracefully.

**Decision.** (2) for milestone 1. The agent speaks only the controller
protocol, so it can later be embedded in (or replaced by) the Kotlin service
without controller changes. Revisit before relying on long unattended runs.

## ADR-002: v0 transport is JSON over HTTP

**Problem.** The target protocol is Protobuf/gRPC. Milestone 1 has three small
messages and benefits from being curl-able while the design settles.

**Decision.** JSON/HTTP, with wire types isolated in `proto/v0.go`, flat
protobuf-compatible fields and explicit units. Move to gRPC + mTLS before any
non-localhost deployment. Development traffic only goes over `adb reverse`
(USB/localhost); unauthenticated RPC is never acceptable in production.

## ADR-003: Phones reach the controller through `adb reverse`

**Problem.** Phones need a route to the controller without Wi-Fi setup.

**Decision.** The provisioner runs `adb reverse tcp:18080 tcp:18080`; the agent
always talks to `127.0.0.1:18080`. Works over USB and TCP adb alike, needs no
IP configuration and keeps traffic off the LAN. Limitation: the phone must stay
attached to the host running adb. Wi-Fi/Ethernet transport is a later
milestone.

## ADR-004: redroid as the development "phone"

**Problem.** Need to test the adb provisioning path before owning phones. The
dev host is Apple Silicon M2 (no nested virtualisation), so an AVD/QEMU
emulator inside Docker would be pure software emulation and far too slow.

**Decision.** Use redroid (Android userspace in a container on the colima VM's
arm64 kernel, with the `binder_linux` module). It runs real arm64 Android with
real `adbd`, so pcprov and node-agent run unmodified. Phone size is emulated with
cgroup limits, which the agent honours. See `docs/DEV_EMULATION.md` for what
this does *not* model.

## ADR-005: llama.cpp as a static musl arm64 binary (no NDK) for first LLM tests

**Problem.** The target native runtime is built with the Android NDK.
The dev host has no NDK, and the Linux NDK is x86_64-only, so it cannot run in
the arm64 build VM.

**Alternatives.** (1) Install the macOS NDK on the host. (2) Build llama.cpp
statically against musl in an Alpine arm64 container.

**Trade-offs.** (2) is reproducible in Docker and runs on any arm64 Android,
since it does not use bionic or `/system` libraries. It cannot use Android-only
APIs (Vulkan/OpenCL GPU backends, NNAPI). musl malloc may be slower. The build
targets `armv8.2-a+dotprod+fp16` (Snapdragon 855 and newer). The Snapdragon
845 (Kryo 385) has FP16 but no dotprod; it crashed with SIGILL on this build
and needs `ARM_ARCH=armv8.2-a+fp16`.
The smoke test checks `/proc/cpuinfo` for `asimddp` before running.

**Decision.** (2) for CPU-only smoke tests (`runtime/llama/Dockerfile`,
`make llama`). Switch to an NDK build when GPU backends or the Kotlin service
arrive.


**Update (2026-09).** The smoke test no longer only checks for `asimddp`:
like pcprov it picks the fastest built variant the CPU supports (ADR-007).
The Makefile's `make llama` still defaults to `armv8.2-a+dotprod+fp16`;
`make llama-all` builds all three variants.

## ADR-006: Inference gateway in the controller, reaching phones over adb forward

**Problem.** Clients need one OpenAI-compatible endpoint for the whole
cluster, not one per phone. The gateway must later support API keys, quotas
and other scheduling policies without a rewrite.

**Alternatives.** (1) Clients call phones directly (per-phone `adb forward`).
(2) Gateway on the phones' LAN IPs over Wi-Fi. (3) Gateway in the controller,
reaching each phone through an `adb forward` on the host that runs adb.

**Trade-offs.** Bandwidth is not the deciding factor: requests and responses
are kilobytes, and generation speed is limited by the phone's CPU. (3) needs no
Wi-Fi setup, keeps `llama-server` bound to the phone's 127.0.0.1 (not exposed on
the LAN) and charges the phones over the same cable. Its cost: every phone must
be attached to the adb host, and that host is on the request path.

**Decision.** (3). The node agent supervises `llama-server` and reports
`{model, ready, advertise_port}` in heartbeats. `advertise_host` is left for
Wi-Fi transport later. The gateway (`controller/gateway`) is split into
replaceable parts:

- `Authenticator`: `AllowAll` (dev) or `StaticKeys` (`-api-keys-file`, only
  SHA-256 hashes kept in memory). The `Principal` it returns is where tenant,
  quota and allowed models will go.
- `Picker`: `Affinity` (default) keeps requests that share a prompt prefix
  (first message + tool definitions, hashed) on one node, so llama-server's
  prompt cache is reused. New large sessions (body ≥ 16 KB) go to the least
  busy node, then the one with the fewest large sessions, then the fastest
  (benchmark score). A pinned node that is 2+ requests busier than the least
  busy one is skipped (spill). `LeastInflight` remains available.
- `BackendSource`: derived from the registry; a different transport only
  changes how URLs are built.

Failure handling: a failed attempt (refused connection or 5xx) is retried once
on another node, and the failed node is avoided for 30 s. Requests in flight on
a node that leaves the ready set (e.g. SUSPECT) are cancelled and retried
elsewhere. Once response bytes have reached the client, a request cannot be
retried.


**Update (2026-09).** Changed since this ADR: the speed tie-break is the
node's measured llama.cpp self-test tok/s, falling back to the synthetic
benchmark only when no node has one, and hot nodes get no new sessions
(ADR-010). Requests can name `auto`, `pool/<name>` or `node/<alias>`, pools
may use a `Spread` picker, and node targets never fail over (ADR-014).
Nodes whose context is smaller than the estimated prompt are skipped, and a
prompt larger than every node's context gets 400 `context_length_exceeded`;
a node that rejects a prompt as too long is not marked down. Switching
nodes (downloading or loading a model) are skipped (ADR-011).

## ADR-007: pcprov selects the llama.cpp build from the phone's CPU features

**Problem.** ADR-005's build targets `armv8.2-a+dotprod+fp16`. A real Xiaomi Mi
8 (Snapdragon 845) turned out to lack dotprod (`asimddp`) entirely, so that
build hits SIGILL on it; only `armv8.2-a+fp16` runs. Requiring the operator to
pass `-llama-server bin/llama-<variant>/llama-server` by hand does not scale
to a drawer of mixed phones.

**Alternatives.** (1) Ship one lowest-common-denominator build (`armv8-a`,
no dotprod/fp16) for every phone. (2) Build a few variants and pick the right
one per phone automatically.

**Trade-offs.** (1) is simplest but throws away real speedups on newer SoCs.
(2) needs `make llama-all` (three Docker builds instead of one, a few extra
minutes) and a bit of selection logic, but every phone gets the fastest build
it can run without any manual flag.

**Decision.** (2). `make llama-all` builds `armv8.2-a+dotprod+fp16`,
`armv8.2-a+fp16` and `armv8-a` into `bin/llama/<ARM_ARCH>/`. During
provisioning, `pcprov` reads `/proc/cpuinfo`'s `Features` line over adb and
picks the fastest of these three variants whose CPU requirements are met and
which was actually built, logging the choice; a phone matching none (missing
build) gets a clear error naming its features and the available builds. The
`-llama-server` flag becomes an explicit override that skips this selection.
The chosen variant is passed to node-agent (`-runtime-variant`) and reported
in heartbeats as `engine: "llama.cpp/<variant>"`, so Grafana/the dashboard can
tell which build each phone runs.

## ADR-008: Admin API and usage statistics

(Numbered 008 because ADR-007 is reserved for llama.cpp variant selection,
which is being written on a parallel branch.)

**Problem.** Operators need to take a phone out of rotation, remove dead
nodes, issue and revoke API keys, change routing, and see usage per key and
per node. Until now this needed a restart, hand-edited files, or a
Prometheus query. A management surface must not become the weakest point of
a cluster that already serves unauthenticated agent traffic in development.

**Alternatives.**
1. Only Prometheus and config files: restart to change anything.
2. Admin routes on the gateway's own auth, so an API key with an "admin" flag
   could manage the cluster.
3. A separate `/admin/` JSON API with its own bearer token, disabled when no
   token is configured, and a CLI (`pbctl`) on top of it.

**Trade-offs.** (1) keeps the attack surface smallest, but every drain means a
restart that interrupts every session. (2) mixes tenants and operators: one
leaked client key could manage the cluster, and "open" dev mode would make
the admin API open too. (3) adds one secret to handle, but the two roles stay
separate and the default is safe: no token means no admin API.

**Decision.** (3).

- **Auth.** `-admin-token-file` holds one token of at least 16 characters.
  The controller keeps only its SHA-256 and compares the hash of the
  presented token with `subtle.ConstantTimeCompare`, so the comparison does
  not leak the token's length or content through timing. Without the flag,
  every `/admin/` route returns 503 with a message naming the flag. Unknown
  admin paths also require the token. Every state change is logged as
  `admin action` with `principal=admin`, the action and its target (node id
  or key name). Every call is counted in
  `phoneborg_admin_actions_total{action,result}`. Tokens and keys never
  appear in logs, metrics or list responses. The controller warns when a
  secrets file can be read by group or others.
- **Nodes.** Drain is a flag in the registry, keyed by node id. It is not a
  node state, so the heartbeat state machine stays unchanged, and it
  survives re-registration and forget. Drained nodes stay in the
  `BackendSource` with `Backend.Drained` set. The gateway does not route new
  requests to them, but `Reap` leaves their in-flight requests alone, so
  those finish normally. Their affinity pins are dropped, so sessions re-pin
  on their next request. The drain flag is not persisted.
- **API keys.** Key files take `<name> sha256:<hex> [created]` next to the
  legacy `<name> <key>` lines. Created keys are `pb-` plus 32 random bytes
  (base64url). The plaintext is returned once and never stored. Changes are
  written to a temp file with mode 0600, synced, and renamed over the old
  file. Legacy lines are rewritten as hashes on the first write. SIGHUP
  reloads the file, and a broken file leaves the current keys in place. An
  empty key file is valid and rejects every request (fails closed).
- **Open gateway and the first key.** Without `-api-keys-file` the gateway
  is open. Creating a key there does not lock anyone out: known keys are
  attributed to their owner, and other requests are still served as
  `anonymous`. Enforcement is a separate, explicit step
  (`PUT /admin/gateway {"auth_mode":"keys"}`, `pbctl gateway set auth=keys`).
  It is refused while no key exists. The switch goes one way only, from open
  to keys. Turning authentication off needs a restart without
  `-api-keys-file`, so a stolen admin token cannot quietly open the gateway.
  Keys created without a file are kept in memory only, and the API response
  says so.
- **Usage.** The gateway reports one `UsageEvent` per outcome, through a
  `UsageRecorder` interface, at the point where it already counts tokens.
  The controller aggregates events per key and per node: since start, and,
  with `-state-dir`, since first use. The totals are persisted to
  `usage.json` (atomic write, mode 0600) every 30 s and on SIGINT/SIGTERM,
  after the HTTP server has drained for up to 10 s. A corrupt file stops
  startup instead of being overwritten. Prometheus stays the source for
  rates and time ranges, and `usage.json` is for all-time totals.
- **Runtime gateway settings.** The routing policy (`affinity` or
  `least_inflight`), the affinity spill threshold and the upstream timeout
  can be changed through `PUT /admin/gateway`. An update is validated as a
  whole and then applied, so a bad field changes nothing. The picker and the
  timeout are swapped atomically, and requests that are already routed keep
  their policy and deadline. The `Picker`, `Authenticator` and
  `BackendSource` boundaries from ADR-006 are unchanged. The settings are not
  persisted: after a restart the controller uses its flags again.
- **pbctl** lives in `controller/cmd/pbctl`, not in a separate `ctl/`
  module. It imports the admin API's request and response types from
  `controller`, so the CLI and the server cannot drift apart, and it is
  versioned and tested with the API it calls (its tests run it against a
  real in-process controller).

**Not solved.** There is one admin role with no per-operator identity, and
no TLS: run the admin API only on localhost or a trusted network until the
mTLS work (ADR-002). Keys have no scopes, expiry or quotas yet. Drain flags
and gateway settings are lost when the controller restarts.


**Update (2026-09).** ADR-007 (llama.cpp variant selection) has since been
merged, so the numbering note above is only historical. `thermal_limit` was
added to the runtime gateway settings by ADR-010; like the others it is not
persisted.

## ADR-009: llama-server threads on big.LITTLE phones

**Problem.** Phones mix fast and slow cores. llama.cpp can be held back by slow
"little" cores, so the agent gained `-threads N` and `-threads-policy all|big`
(big = only cores within 90% of the highest max frequency).

**Measurement** (Xiaomi Mi 8, Snapdragon 845, Qwen2.5-0.5B Q4, real agent +
llama-server, 297-token prompt, 64 generated tokens, two interleaved series).
Android places the agent in the `foreground` cpuset (cores 0-3 little, 6-7 big),
so 6 cores are usable:

| threads | prompt tok/s | generation tok/s |
|---|---|---|
| 2 (= policy `big` here) | 13.4 | 8.8 |
| 4 | 19.0 | 9.3 |
| 6 (= policy `all`) | 23.8 | ~12 |

Two traps showed up. Qualcomm `core_ctl` keeps only 2 of the 4 big cores online
when idle, so benchmarks that pin threads with `taskset` stall on offline cores
(0.1–0.5 tok/s) and do not reflect the unpinned server. Thread counts above the
usable cores are also catastrophic.

**Decision.** Default policy stays `all`, capped by the usable cores. `big` and
`-threads` remain as per-phone overrides, to be re-measured on other SoCs.
Benchmark through llama-server, not pinned `llama-bench` runs.


**Update (2026-09).** The measurements are also listed in
[BENCHMARKS.md](BENCHMARKS.md#threads-on-biglittle).

## ADR-010: Measured inference speed and thermal-aware routing

**Problem.** The gateway's `Affinity` picker breaks ties with `Backend.Speed`,
which was the agent's synthetic benchmark (`Benchmark.CPUGFLOPS`, kind
`synthetic-go-v0`, ADR-006/ADR-009). That benchmark is a Go microbenchmark's
FLOPS, with no fixed relationship to llama.cpp tokens/s: a real Xiaomi Mi 8
scores 7.4 GFLOPS and an emulated 2-core phone 8.6, yet real generation is
~12 tok/s on the Mi 8 against 74 tok/s on the emulated phone. Long sessions
(opencode, 11k-token prompts) could land on the wrong node. Routing also
ignored temperature: a hot phone throttles and generates slower, but kept
receiving new sessions like any other.

**Decision.**
- The agent runs one self-test against its own llama-server right after it
  becomes ready, and again after each runtime restart: `POST
  /v1/chat/completions` with a fixed ~100-token prompt, `max_tokens=32`,
  `ignore_eos=true`, `cache_prompt=false`, `temperature=0`, reading
  `timings.prompt_per_second` / `timings.predicted_per_second` from the
  response. Results are reported in `RuntimeStatus` (`GenTPS`, `PromptTPS`,
  `SelfTestAt`), logged as `runtime self-test`, and left at zero if the
  self-test fails (the node still serves). The existing startup benchmark is
  unchanged.
- `controller.Backends()` uses `GenTPS` as `Backend.Speed` when a node has
  reported one, else falls back to the synthetic benchmark. The two are
  different units, so once *any* candidate has a measured speed, nodes
  without one get `Speed 0` rather than being compared on the wrong scale.
- The controller gains `-thermal-limit-c` (default 75, 0 disables). A node
  whose last heartbeat temperature is at or above the limit is `Hot`.
  `Affinity` and `LeastInflight` do not send *new* sessions (misses, or a
  session with no existing pin) to a hot node unless every candidate is hot;
  an existing pinned session spills off a node that turned hot the same way
  it spills off a busy one (ADR-006), provided a cooler candidate exists.
- The limit is a runtime setting like the existing policy/spill/timeout ones
  (`PUT /admin/gateway {"thermal_limit_c":...}`, `pbctl gateway set
  thermal_limit=<c>`, ADR-008) and is not persisted across restarts.
- New metrics: `phoneborg_node_runtime_gen_tokens_per_second` and
  `phoneborg_node_runtime_prompt_tokens_per_second` (the self-test; not to be
  confused with the gateway's existing `phoneborg_node_generation_tokens_per_second`,
  which reflects only the last proxied request), and `phoneborg_node_hot`.
  `pbctl nodes` shows a `TOK/S` column and flags hot nodes in `STATE`.

**Trade-offs.** A self-test after every restart adds one short generation
(well under a second of extra work, off the request path) before a node's
speed is "measured"; until then it still routes correctly by falling back to
the synthetic benchmark. Thermal-aware routing only looks at the last
heartbeat, so a node can still take one new session in the few seconds before
its next heartbeat reports it as hot: a soft protection, not a hard cutoff.
Nodes with no readable thermal zone (e.g. redroid) are never marked hot,
matching the existing gap in `phoneborg_node_temperature_celsius`.

## ADR-011: Model catalog and placement

**Problem.** Every phone served the one GGUF file `pcprov -model` pushed at
provisioning. Serving a different model meant re-provisioning phones one by
one, and nothing matched models to phones: a 3B model on a 3 GB phone gets
killed, a 0.5B model on a 12 GB phone wastes it. The gateway already routes
by model name, so the missing part is deciding and delivering which model
each phone serves.

**Alternatives.**
1. Keep pushing models with pcprov; operators pick per phone.
2. Phones download from Hugging Face themselves.
3. The controller keeps a catalog, plans placement from policies, and phones
   download the file from the controller over their existing link.

**Trade-offs.** (1) needs a person at the adb host for every change. (2)
needs Wi-Fi and working TLS on every phone, downloads each file once per
phone and gives no central view. (3) downloads each model once, keeps
phones on the USB link (ADR-003) and makes placement visible and
auditable, at the cost of disk space on the controller and a new
controller-to-node message.

**Decision.** (3).

- **Catalog** (`controller/models`). Sources are `https://` URLs,
  `hf://<owner>/<repo>/<file>.gguf` (mapped to
  `https://huggingface.co/<owner>/<repo>/resolve/main/<file>`) and
  `file:///abs/path.gguf`. Files are downloaded to `<id>.gguf.part` in
  `-models-dir` (default `<state-dir>/models`), resumed with a `Range`
  request after a dropped connection or a restart, retried three times on
  network errors and 5xx (not on 4xx), aborted after 2 minutes without
  data, hashed (SHA-256) and renamed into place only once the GGUF header
  parses. The metadata is read with
  [gguf-parser-go](https://github.com/gpustack/gguf-parser-go) (MIT): we do
  not maintain our own GGUF parser. Ids are the lowercased file name
  without `.gguf`, so the file pcprov pushes today keeps its name. The
  catalog is `<state-dir>/models.json`; without a state dir it lives in
  memory and files go to a temporary directory.
- **Device classes** by `Inventory.RAMTotalBytes`: `xs` < 3 GiB, `s` 3–5,
  `m` 5–7, `l` 7–10, `xl` 10+ GiB. `max_ram_bytes` is 0 for the open-ended
  `xl`.
- **RAM heuristic.** `est = file size + f16 KV cache + 150 MiB`, with
  `KV = 2 (K and V) × layers × ctx × kv_heads × head_dim × 2 bytes` and
  `ctx = min(16384, ctx_train)`. A model fits a node when
  `est ≤ RAM − 2 GiB` (Android and its resident apps). `fits_classes` uses
  each class's minimum RAM, so `xs` never fits. The estimate ignores
  sliding-window attention (Gemma 3) and compute buffers; it is a
  placement guide, and the agent may lower the context or quantize the KV
  cache (`kv_type: "auto"`) to fit. `fits_classes` on the catalog stays this
  heuristic (it describes a whole class, not a connected node), but the
  planner's actual per-node fit check (below) does better once a node has
  reported one heartbeat: the fixed 2 GiB baseline is only an approximation
  of what Android leaves free, and is wrong in both directions (ADR-012's
  agent-side `MemoryBudget`, measured from `/proc`, found 0.65 GiB used on
  emulated 2–3 GiB phones and 2.4 GiB used on a real Mi 8, against the
  heuristic's flat 2 GiB either way).
- **Node-level fit.** Once a node's heartbeat reports `RuntimeStatus.
  BudgetBytes` (ADR-012), the planner checks fit against that real budget
  instead of the class heuristic: a model fits if its estimate at the
  requested context and an f16 KV cache is within budget, or, mirroring the
  agent's own degradation order, with a q8_0 KV cache, or with the context
  halved down to 4096 (`internal/memplan`, shared with the agent's
  `PlanMemory` so the plan and the agent's own sizing never disagree).
  Nodes that have not yet reported a budget (`BudgetBytes` 0: an older
  agent, or no heartbeat yet) still use the RAM heuristic above. The
  per-node budget is shown in `Placement.nodes[].budget_bytes`, the `pbctl
  placement` `BUDGET` column and the web panel's Placement view.
- **Planner** (`models.Planner`, default `DefaultPlanner`): a pure,
  deterministic function of policies, the default model, nodes (id,
  class, RAM, measured tok/s, current model, previous assignment,
  drained) and ready models. One model per node; pins, then replicas, then
  percent (of nodes eligible by class filter and fit, rounded half up, at
  least 1), then the default model where it fits, otherwise the node
  keeps what it serves. Bigger models choose first; candidates are ordered
  by "already serves it", "was assigned it by the last plan" (so a node
  whose tok/s drops to 0 while switching is not replaced), measured tok/s,
  RAM, id. Drained nodes only follow pins; OFFLINE nodes are not planned.
  Policies for models that are not ready wait, with a warning. Validation
  (unknown model or node, one policy per model, a node pinned twice,
  percentages over 100 for any class) rejects a `PUT` with 400. The plan
  is recomputed on placement changes, node register/forget/drain, model
  ready/removed, and every 10 s. Policies and the default model persist
  in `<state-dir>/placement.json`.
- **Delivery.** `POST /v1/heartbeat` answers 200 with
  `HeartbeatResponse{desired}` when the plan assigns the node a ready model
  (reason pin, replicas, percent or default), and 204 otherwise, so a node
  provisioned with `pcprov -model` and no policy keeps serving as before.
  `DesiredRuntime` carries the file URL (`/v1/model-files/<id>`), SHA-256,
  size, `ctx_size` (16384 capped by `ctx_train`), `slots` 1, `kv_type`
  `auto` and the layer/KV-head/head-dim metadata for the agent's own RAM
  check. `/v1/model-files/` uses `http.ServeContent`: `Range`,
  `Content-Length`, and `ETag` = the SHA-256 in quotes.
- **Routing.** `Backends()` still requires `Ready`, and additionally skips
  nodes whose `RuntimeStatus.State` is `downloading` or `loading`, so a
  switching node gets no requests even if it reports `Ready`. The served
  name (`RuntimeStatus.Model`) equals the model id, so the gateway's
  routing by model name is unchanged.
- **Admin API** `/admin/models` (GET, POST, PATCH, DELETE; DELETE is 409
  while a policy or the default uses the model), `/admin/device-classes`,
  `/admin/placement` (GET, PUT) and `/admin/placement/preview` (POST,
  nothing applied), audited and counted like the other admin actions
  (ADR-008). pbctl: `models`, `classes`, `placement`; the old `pbctl
  models` (list of served models) became `pbctl served`.
- **Metrics.** `phoneborg_model_info{model_id,arch,params,quant}`,
  `phoneborg_model_download_progress{model_id}`,
  `phoneborg_node_model{node_id,model_id,state}` and
  `phoneborg_placement_plan_nodes{model_id}`; a Models row in Grafana.

**Not solved.** `/v1/model-files/` needs no token (the same trust as the
heartbeat, ADR-002): anything that reaches the controller's port can
download catalog models, and a `file://` source makes a controller-side
GGUF file downloadable. Keep the controller on localhost or a trusted
network until mTLS. Hugging Face tokens (gated repositories) and split GGUF
files are not supported. The expected SHA-256 of a download is not
checked against the source (Hugging Face publishes it); the hash only
lets agents verify their copy. Context size, slots and KV type are not
yet configurable per policy.


**Update (2026-09).** The RAM heuristic and the per-node fit check now use
the model's resident bytes instead of the file size (ADR-012 update below),
and placement also excludes nodes whose predicted speed is below
`min_tok_s` / `-min-predicted-tok-s` (ADR-015). The old read-only node table
moved from `/` to `/status` (ADR-013).

## ADR-012: Agent-side model switching and memory sizing

**Problem.** The controller is gaining a model catalog and a placement
planner (parallel work) that assigns each node a model over heartbeats. The
agent must switch `llama-server` to that model without a replug, choose a
context size, slot count and KV cache type that actually fit the phone's RAM,
and keep serving through a bad switch.

**Decision.**

- **Contract** (`proto/v0.go`). A heartbeat may still get a 204 (no desired
  state, or an old controller); a 200 carries `HeartbeatResponse{Desired
  *DesiredRuntime}`. `DesiredRuntime` names the model id (also the served
  name, `llama-server -a`), a controller-relative download URL, its
  SHA-256/size, the GGUF shape (`Layers`, `KVHeads`, `HeadDim`) needed for the
  RAM estimate, and the requested `CtxSize` (per slot), `Slots` and `KVType`
  ("f16" | "q8_0" | "auto"). `RuntimeStatus` gains `ModelID`, `State`
  ("serving" | "downloading" | "loading" | "error" | "idle"), `Progress`,
  `Error`, `Slots`, `KVType`, `RAMEstimateBytes`. `ModelID` equals `Model`
  once serving, and is set to the switch's target as soon as a
  download/load starts (State `downloading`/`loading`), so the placement
  planner can tell a node is already claimed before it is ready; the gateway
  skips a node in that state even though `Ready` (which keeps reflecting the
  previous model, still actually serving) is true.
- A `Manager` (`node-agent/manager.go`) owns the active `Runtime` (the
  existing supervisor; its restart/backoff/pidfile logic is unchanged) and
  reconciles one desired state at a time in its own goroutine: `SetDesired`
  keeps only the latest value from each heartbeat (a channel of size 1), so a
  slow switch is never queued twice, and reconciling a `DesiredRuntime` equal
  to the last one applied is a no-op.
- **Download** (`node-agent/download.go`): `models/<model_id>.gguf.part`,
  resumed with an HTTP `Range` request if a partial file already exists,
  verified against the full SHA-256 (not the controller's ETag) once
  complete, then renamed into place. A file already on disk with the right
  hash is never re-downloaded. Free storage is checked with `statfs` first;
  if short, other cached `.gguf` files are deleted oldest-mtime-first (the
  model being loaded touches its own mtime, making this a real LRU), never
  the model currently served; if still short, the switch fails with `error`
  before touching the network.
- **Memory sizing** (`node-agent/sizing.go`, pure, table-tested): `budget =
  MemAvailable + RssAnon(current llama-server) − reserve` (`-mem-reserve-mb`,
  default 600 MiB). `need(ctx, slots, kv) = file size + slots·ctx·bytesPerToken(kv)
  + 150 MiB`, `bytesPerToken = 2·Layers·KVHeads·HeadDim·bytesPerElement(kv)`
  with `bytesPerElement` 2 for f16 and 1.0625 for q8_0 (32-element blocks
  plus one f16 scale each). `KVType "auto"` tries f16 then q8_0 (q8_0 forces
  `-fa on`, required for its V-cache); if nothing fits, ctx halves down to
  4096, then slots drops to 1; if still nothing fits, the switch fails with
  `"model needs X MiB, budget Y MiB"`.

  *RssAnon, not total RSS.* llama-server `mmap`s the GGUF file, so most of
  its RSS is the weights' file-backed pages, which the kernel already
  reports as reclaimable in `MemAvailable`. Measured on a Mi 8,
  `MemAvailable` barely moves when a model is loaded (no model: 3156 MiB;
  Qwen2.5-0.5B: 2984 MiB; Qwen2.5-1.5B: 2970 MiB), confirming those pages are
  already counted as available. Adding the server's full RSS on top would
  double-count them; `RssAnon` (KV cache and compute buffers, not
  file-backed) is the part that only becomes free once the process actually
  exits. Measured RSS at `-c 8192 -np 1` (file size in parentheses):
  Qwen2.5-0.5B 632 MiB (468), Gemma-3-1B 926 MiB (768), Llama-3.2-1B 1120 MiB
  (770), Qwen2.5-1.5B 1375 MiB (1065) — an `RssAnon` of roughly 150–350 MiB
  per model at that context, not the much larger number full RSS would
  suggest.
- **Loading**: the previous `llama-server` is stopped (its context is
  cancelled, which sends `SIGTERM`; the existing `cmd.WaitDelay` still
  backstops a hard kill) and a new one started with `-m models/<id>.gguf -a
  <model_id> -t <threads> -c <ctx*slots> -np <slots>` (plus `-ctk q8_0 -ctv
  q8_0 -fa on` when the chosen KV type is q8_0). **`-c`/`--ctx-size` is
  llama-server's *total* context across every `-np` slot** (each slot gets
  `ctx-size / n_parallel`), not a per-slot size, so the agent multiplies the
  per-slot `CtxSize` by `Slots` when building `-c`. (This matches llama.cpp's
  documented server semantics and the shared contract's own `-c <ctx*slots>
  -np <slots>` wording; it was not re-verified against a running
  `llama-server --help` in this environment, since no llama.cpp binary or
  vendored source exists here outside the Docker build in `runtime/llama/`.
  Verify on a real phone: two concurrent requests at a known `-c`/`-np`
  should each see roughly `ctx-size/n_parallel` tokens of context, not the
  full `-c` value.) The new server is `serving` once `/health` is 200, at
  which point the existing self-test (ADR-010) reruns automatically,
  unchanged.
- **Failure and fallback**: a failed download or sizing failure never
  touches the running server, so the node keeps serving the old model with
  no extra logic. A failed *load* (the one step that must stop the old
  server first) restarts the previous model from its still-cached file, if
  any survived eviction — always true, since eviction never removes the
  currently-served model. Either way, `State` is `error` with a message
  naming the model and reason, and `RuntimeStatus.Model`/`ModelID` reflect
  whatever is genuinely running (the old model on fallback, nothing if there
  was none to fall back to).
- **Static mode is unchanged**: a node started with pcprov's
  `-model`/`-ctx-size` serves it immediately, exactly as before, until the
  controller sends a `DesiredRuntime` — which then takes over permanently
  (even naming the same model, since only the controller-driven path picks
  slots/KV type and re-verifies the file's hash).

**Trade-offs.** Halving context and dropping to one slot are coarse
degradation steps; a controller that wants fine-grained control over context
should ask for what it actually wants rather than relying on this per-node
fallback. Eviction is LRU by file mtime (touched on load), not a true
access-time log, so a model downloaded but never actually loaded (e.g. it
lost a placement race) looks "recently used" until something else evicts it.
The 2-minute timeout for a new `llama-server` to answer `/health` is a fixed
constant, not configurable; slow phones with large models may need it
revisited.

**Not solved.** No cross-node coordination of which node evicts what cached
model (each agent only ever protects its own currently-served model); no
bandwidth limiting on downloads sharing the same USB link as request
traffic; `-mem-reserve-mb` is a single flag, not learned from observed OOM
kills.

### Update (2026-09): resident bytes, not file bytes (Gemma 3n)

(Referred to in code comments as the "ADR-012 addendum".)

**Problem.** Sizing assumed the whole GGUF file must be resident. Measured on
a real Xiaomi Mi 8, Gemma 3n E2B it (Q4_K_M, file 2886 MiB) kept a huge
per-layer embedding table (`per_layer_token_embd.weight`, 1440 MiB) that
llama.cpp reads only a few rows of per token through mmap: measured
llama-server RSS was 1774 MiB, not anywhere near the file size. The agent
still refused the model ("model needs 3164 MiB, budget 2853 MiB") even though
it ran fine, because both the agent's `PlanMemory` and the planner's fit
check counted `SizeBytes` as fully resident.

**Decision.** The catalog (`controller/models.ReadMeta`) computes
`sparse_bytes`: the total on-disk bytes of tensors llama.cpp accesses
sparsely by row. The rule (`isSparseTensor`, `controller/models/gguf.go`) is
a tensor name ending in `token_embd.weight` and starting with `per_layer_`
(i.e. `per_layer_token_embd.weight`), plus a short, explicit, extensible
table for any other architecture measured later. Plain `token_embd.weight` is
deliberately never matched: when a model ties its input and output
embeddings (no separate `output.weight`), the output layer reads it in full
on every forward pass, so treating it as sparse would undercount resident
memory; for simplicity this addendum only covers the per-layer case, which is
the one measured. `resident_bytes = size_bytes - sparse_bytes` is exposed on
`Model` next to `sparse_bytes`, and both are computed once at download time
and, for a catalog saved before this addendum existed, lazily recomputed from
the file's already-downloaded GGUF header the next time the controller starts
(no re-download).

`DesiredRuntime` gains `ResidentBytes` (0 = unknown, agents fall back to the
file size), filled from the catalog. `internal/memplan` (shared by the
agent's `PlanMemory` and the planner's per-node fit check) uses resident
bytes for the weights term instead of the file size, plus
`SparseAllowanceFrac` (10%, a documented constant) of the sparse bytes, for
the mmap pages a session actually touches — not zero, because some rows are
read, and not the full size, which would recreate the original bug. The
agent still reports `RuntimeStatus.ModelBytes` as the file size (ADR-015
depends on this for nodes with no catalog entry), but also reports
`ResidentBytes` for the currently served model, so the controller's bandwidth
estimate (ADR-015) can use it too.

**Trade-off.** 10% of sparse bytes is a deliberately simple approximation,
not fit to the one Gemma 3n measurement; it is easy to revisit once more
architectures with sparse tensors are measured. The catalog's older, simpler
RAM heuristic (`EstimateRAM`, ADR-011) also switched to resident bytes, but
without the 10% allowance, since it already ignores compute buffers and
sliding-window attention and is only a placement guide.

### Update (2026-09): retry backoff and keeping a running model

Two behaviours were added after the ADR above. A switch to the same desired
state that just failed is retried after 30 s, doubling up to 10 minutes,
or earlier once the node's RAM budget has grown by more than 10%; before,
the agent retried on every heartbeat. And when the requested settings do
not fit but the requested model is already running (for example started
by `pcprov -model` with a smaller context), the agent keeps that instance
and reports no error instead of failing the switch.

## ADR-013: Web management panel

**Problem.** Managing the cluster meant `pbctl`, curl, Grafana and the old
`/` table side by side. Model placement adds editing that a CLI does poorly:
choosing nodes, percentages and class filters, and checking what a change
does before applying it. Operators want one place for nodes, models,
placement, gateway settings, API keys and usage.

**Alternatives.**
1. A single-page app with a framework and a build step (React, Svelte),
   bundled into the controller.
2. Server-rendered HTML templates with forms that post to new endpoints.
3. Static HTML, CSS and vanilla ES modules, embedded with `go:embed`, that
   call the existing admin API from the browser.

**Trade-offs.** (1) gives the richest UI, but adds a Node toolchain, a
lockfile and generated assets to a Go repository, and a supply chain to
review. (2) keeps everything in Go, but duplicates the admin API as HTML
handlers, and every change to the API needs a matching form handler. (3)
has no build step and no new server code paths: the panel is just another
admin API client, like `pbctl`, so the API stays the only place where
authentication, validation and audit logging happen. Its cost is a little
DOM code written by hand.

**Decision.** (3), in `controller/ui`.

- **Serving.** `GET /ui/` serves the embedded files. `/` redirects to `/ui/`,
  and the old read-only table moved to `/status` (no login, for quick
  checks). The panel works offline: no CDN, no web fonts. Responses carry a
  strict Content-Security-Policy (same-origin scripts, styles and requests
  only, no inline code, no framing), `nosniff` and `no-referrer`, and
  directory listings are disabled.
- **Auth.** The operator types the admin token. It is kept in
  `sessionStorage` (this tab only, gone when the tab closes), sent as
  `Authorization: Bearer`, and never put in URLs. A 401 returns to the login
  screen; a 503 shows that the admin API is disabled. The static files
  themselves need no token: they hold no data.
- **Rendering.** Server strings only ever become DOM text nodes or attribute
  values (a small `h()` helper), never HTML, so node names, model ids and
  warnings cannot inject markup.
- **Live data.** Live views poll every 5 s while the tab is visible and no
  dialog is open. Requests and tokens per second are derived in the browser
  from successive `/admin/stats` totals; no new endpoint was added.
- **Placement.** Policy edits stay local until previewed with
  `POST /admin/placement/preview`; Apply (`PUT /admin/placement`) is enabled
  only for the exact state that was previewed. Views for model management
  (models, device classes, placement) show a "not available" state when the
  controller answers 404, so the panel also works with controllers that do
  not have that API.
- **Links.** Grafana and Prometheus links default to ports 3000 and 9090 on
  the controller's host, and can be set in a settings dialog or with
  `/ui/?grafana=URL&prometheus=URL` (kept in `localStorage`).

**Not solved.** No TLS: the admin token crosses the network in clear text
unless the panel is used on localhost or behind a TLS proxy (ADR-008).
There are no per-operator accounts or audit identity beyond
`principal=admin`. Browser tests are manual; the Go tests check only that
the files are embedded, served with the right types, and reference no
missing file.

## ADR-014: Virtual models and pools for agent workloads

**Problem.** Agent tools such as opencode run several subagents at once,
and each subagent names one model. With only served model ids, every
subagent competes for the same phones under session affinity: all requests
for a model go to the phones serving it, and there is no way to give the
reviewer agent the fast phones, keep a scratch agent on one spare phone,
or send a group of small agents to "whichever phones are free". Node ids
(`mi8-6f3a…`) are also too unfriendly to put in an agent's config.

The workloads also differ. A tool-less custom agent sends about 180 prompt
tokens per request, whereas opencode's default build agent sends about
11.7k (system prompt plus tool definitions), measured with opencode 1.18.30.
On a phone the first costs seconds of prompt processing cold, the second
around 15 minutes. Session affinity (ADR-006) exists to protect the second
kind; for the first it only serialises work that could run in parallel.

**Alternatives.**
1. Separate gateway ports or base URLs per group of phones.
2. Use placement (ADR-011) to give each group its own model, and address
   groups by model id.
3. Virtual model names resolved by the gateway: `auto`, `pool/<name>` and
   `node/<alias-or-id>`, next to plain model ids, with pools and aliases
   managed through the admin API.

**Trade-offs.** (1) needs a listener and a provider entry per group and
cannot express a single phone without one port per phone. (2) ties routing
to what phones serve: two groups serving the same small model would need
two copies under different ids, and a phone cannot be addressed on its own.
(3) keeps one endpoint and one provider entry, works with every OpenAI
client (a model id is just a string; opencode accepts ids that contain
`/`), and leaves placement alone. Its cost is a naming convention inside
the model namespace: aliases may not start with `pool` or be `auto`, and
catalog ids should not start with `pool/` or `node/`.

**Decision.** (3).

- **Resolution.** The gateway resolves the request's `model` before
  choosing candidates: `auto` is any ready node, `pool/<name>` the pool's
  eligible members, `node/<alias-or-id>` one node, anything else a served
  model as before. The controller implements pools and nodes behind a
  `Targets` interface; the gateway handles `auto` and model ids itself.
  Unknown pools and nodes get 404 `model_not_found`. The upstream body is
  unchanged (llama-server ignores `model`), the `model` metric label keeps
  the real served model, and `phoneborg_gateway_target_requests_total
  {target}` counts `auto`, `pool/<name>`, `node/<alias-or-id>` and `model`.
- **Node targets** bypass the picker and are never retried on another node:
  an agent that asks for one phone wants that phone's cache and behaviour,
  not a silent substitute. A node that is not ready or drained gets 503
  `node_unavailable`; a failed attempt ends in 502. The context-size check
  still applies. A hot node is still served, consistent with ADR-010's rule
  that a hot phone is used when it is the only candidate.
- **Pools** filter by served model, node (alias or id), device class and
  self-test tok/s (`min_gen_tps`, ADR-010). Hot, drained, not ready and
  switching nodes are never eligible. Responses list every node with
  `eligible` and a `reason`, so an empty pool explains itself.
  `phoneborg_pool_members{pool}` exports the eligible count.
- **Spread vs affinity.** A pool's `routing` is `spread` (default) or
  `affinity`. `Spread` is a new `Picker`: least in-flight, then highest
  speed, then rotation, with no session state; it never reads or updates
  affinity pins. Pools are meant for small, tool-less agents whose prompts
  are about 180 tokens: recomputing such a prompt costs a phone a few
  seconds, so running N agents on N phones at once beats queueing them on
  the phone that happens to hold their cache. Agents with long prompts
  (the 11.7k-token build agent) should use `affinity`, which routes exactly
  like a plain model name under the gateway's policy. The `Picker`
  interface is unchanged; a target may override the gateway's picker.
- **Aliases** are kept by node id in the registry, like drain flags, so they
  survive re-registration and forget. They must match
  `^[a-z0-9][a-z0-9-]{0,31}$`, be unique, and not equal another node's id.
  `proto.Node` gains an additive `alias` field. Unlike drain flags, aliases
  and pools are persisted, to `<state-dir>/routing.json` (atomic write, mode
  0600), because agent configs refer to them by name.
- **`/v1/models`** gives every entry a `kind` (`model`, `auto`, `pool`,
  `node`) and lists `auto`, every pool and every aliased node after the
  served models, so clients can discover targets. Unaliased nodes are not
  listed; `node/<id>` still routes.
- **Prewarm.** `POST /admin/prewarm` sends every eligible node of a target,
  in parallel, the given messages and tools plus a final user "ok", with
  `max_tokens: 1` and `cache_prompt: true`, so each phone caches the prefix
  before an agent run. Each node gets up to 120 s. The request counts as
  in flight on the node, so pickers treat it as busy meanwhile.

**Not solved.** Pool membership is evaluated when a request arrives; a
node that turns hot or starts switching mid-request finishes that request.
There are no per-key restrictions on targets (any key may use any pool or
node) and no quotas per pool. Renaming an alias silently removes the node
from pools that list the old alias. Prewarm does not pin anything, so under
`affinity` routing the first real request may still land on another node.

## ADR-015: Performance tiers from measured bandwidth

**Problem.** Devices are classed only by RAM (ADR-011's `xs`..`xl`). A phone
with enough RAM but a slow CPU/memory system (an older SoC, a busy background
app) can be assigned a model it technically fits but runs uselessly slowly.
Nothing in placement looks at speed, only fit.

**Measured fact.** On phones, token generation is memory-bandwidth bound: the
model's weights stream from RAM (or the mmapped file) once per generated
token, so `gen_tok_s * model_file_bytes` is roughly constant for a given
phone ("effective bandwidth"), regardless of which model is loaded. Measured
on a Xiaomi Mi 8 (Snapdragon 845, ADR-009/REAL_PHONES.md): Qwen2.5-0.5B (468
MiB) 14.8 tok/s -> 6.9 GB/s; Qwen2.5-1.5B (1065 MiB) 6.8 tok/s -> 7.2 GB/s;
Gemma 3 4B (2374 MiB) measured 2.4 tok/s against 3.0 predicted from that
bandwidth (the gap is sliding-window attention and compute buffers the simple
model ignores, ADR-011). An emulated phone on an M2 host is a different
regime entirely: ~120 tok/s x 0.47 GB = 56 GB/s. Prompt processing scales the
same way, more noisily: `prompt_tok_s * file_bytes` is roughly constant too
(Mi 8: 36.2 x 0.49 GB = 17.7; 11.4 x 1.12 GB = 12.7).

This needs no phone lists or hardware database: it falls out of the self-test
the agent already runs after every model switch (ADR-010).

**Decision.**

- **Measurement** (`controller/models.Bandwidth`, `PredictedTPS`,
  `PerfTierOf`). The agent reports `RuntimeStatus.ModelBytes`, the size of
  the file it currently serves, read from the file itself (`os.Stat`), not
  looked up in the controller's catalog — so a node started with plain
  `pcprov -model` and no catalog entry also reports it. Combined with the
  existing self-test (`GenTPS`, `PromptTPS`, ADR-010), the controller
  computes `gen_gbps = GenTPS * ModelBytes / 1e9` and `prompt_gbps` the same
  way, every heartbeat.
- **Per-node state** (`controller.nodePerf`). The controller keeps the last
  known-good `gen_gbps`/`prompt_gbps` per node id, the same way it keeps
  aliases and drain flags (ADR-008/ADR-014): a heartbeat with no fresh
  self-test yet (mid switch, `GenTPS`/`PromptTPS`/`ModelBytes` still 0) never
  clears a previous good value. It is persisted in `routing.json`'s new
  `performance` field, alongside aliases and pools, so a controller restart
  does not lose it; it is naturally re-measured after every model switch,
  since that is when the agent's self-test reruns.
- **Performance tiers**, by `gen_gbps`: `t1` < 4, `t2` 4-10, `t3` 10-25, `t4`
  >= 25 GB/s; `"?"` when a node has not measured its bandwidth yet.
  `perf_tier`, `gen_gbps` and `prompt_gbps` are exposed on `AdminNode`,
  `PlacementNode` and `/admin/device-classes`, which gains a `perf_tiers`
  list with node counts (`classes` is unchanged).
- **Predicted speed**, per (node, model): `pred_gen_tps = gen_gbps * 1e9 /
  model.size_bytes`, `pred_prompt_tps` the same way with `prompt_gbps`. Added
  to plan entries (`pred_gen_tps`, `pred_prompt_tps`; 0 when unknown).
- **Planner exclusion.** A node is not eligible for a model when its
  predicted generation tok/s is below a threshold: a policy's own
  `min_tok_s` (new optional `Policy` field), or else the controller's
  `-min-predicted-tok-s` flag (default 3, `models.DefaultPlanner.MinTokS`).
  An unknown prediction (the node has not measured its bandwidth) never
  excludes — a newly connected phone is not penalized before its first
  self-test. Pins are not blocked by speed, the same way they are not
  blocked by a memory-fit shortfall (ADR-011): they still place, with a
  warning ("gemma-3-4b on mi8: predicted 3.0 tok/s < 4"). Replicas and
  percent policies simply do not count a too-slow node as eligible; if that
  empties the pool, the existing "0 nodes" warning names the reason. This is
  about placing *models*; pools' existing `min_gen_tps` (ADR-014, the
  measured speed of whatever a node already serves) is unrelated and
  unchanged.
- **Catalog.** Models gain an optional `recommended_tiers` list, exactly
  like `recommended_classes`: editable via `PATCH /admin/models/{id}` and
  `pbctl models recommend <id> classes=s,m tiers=t2,t3` (the older `pbctl
  models recommend <id> s,m` form, classes only, keeps working).
- **CLI and panel.** `pbctl nodes` shows class and tier together (`m/t2`);
  `pbctl classes` shows both the RAM-class and the performance-tier tables;
  `pbctl placement` gains a `PRED TOK/S` column. The web panel shows
  class/tier in the Nodes and Placement views, predicted tok/s in the
  placement plan table, a min-tok/s field in the policy editor, and
  recommended tiers in the model edit form.

**Trade-offs.** Effective bandwidth is a simplification: it ignores prompt
processing's different compute/memory balance (hence the separate, noisier
`prompt_gbps`) and architecture differences in compute-per-byte (attention
variants, MoE). It is good enough to stop a node from being handed a model
that is *categorically* too slow for it, not a precise performance model.
Because the threshold uses a *predicted* speed from another model's
bandwidth, a newly added, never-tried model can be excluded (or wrongly
allowed) based entirely on file size; the agent's own self-test after the
switch is still the ground truth; ADR-010's `Backends()` speed comparison,
used for routing already-served models, is untouched by this ADR.

**Not solved.** No tier ever downgrades a node's *current* assignment mid-flight;
exclusion only affects future placement decisions. A node's bandwidth is a
single number carried across every model it might serve, which is a
simplification for architectures with unusual attention or MoE patterns
(the RAM heuristic has the same kind of gap, ADR-011). There is no
UI/CLI warning when a policy's `min_tok_s` is set below what any connected
node can ever reach.

### Update (2026-09): bandwidth from resident bytes, not file bytes

**Problem.** `gen_gbps = GenTPS * ModelBytes` overstates bandwidth for a model
like Gemma 3n E2B, whose file is much bigger than what llama.cpp actually
keeps resident (ADR-012's addendum): the same generation speed divided by a
too-large byte count understates the phone's real bandwidth, and dividing a
*candidate* model's file size by that understated bandwidth then mispredicts
its speed on other nodes.

**Decision.** `gen_gbps`/`prompt_gbps` are computed from
`RuntimeStatus.ResidentBytes` when the agent reports one (falling back to
`ModelBytes`, the file size, when it does not — an older agent, or static
mode). Predicting a candidate model's speed (`PredictedTPS`) likewise divides
by that model's `resident_bytes` from the catalog, falling back to
`size_bytes` when unknown. Both functions are otherwise unchanged: they take
whatever byte count the caller passes, so this is purely a change in what
`controller.nodePerf.update` and the planner pass in, not in `Bandwidth` or
`PredictedTPS` themselves.

**Trade-off.** None of this changes for models with no sparse tensors
(`resident_bytes` equals `size_bytes` there), so every number in the table
above is unaffected; it only corrects the Gemma-3n-shaped case ADR-012's
update measured.

### Update (2026-09): bandwidth arithmetic

The Mi 8 generation bandwidths quoted under "Measured fact" (6.9 and
7.2 GB/s) treated MiB as 10^6 bytes. In bytes, as the code computes them,
they are 7.3 and 7.6 GB/s (`controller/models/perf_test.go` checks 7.59).
The tier (`t2`) and the conclusions are unchanged. See
[BENCHMARKS.md](BENCHMARKS.md#effective-bandwidth-and-performance-tiers).

## ADR-016: External engine nodes

**Problem.** Phones run small models. A developer who also has a Mac with
LM Studio, oMLX or Ollama, or a PC with llama-server, wants one endpoint for
both: for example an opencode primary agent on a 14B model on the Mac and
its tool-less subagents on phone pools. Today only phones running the node
agent can be nodes, so such a client needs two providers, and the Mac's
model gets none of the gateway's keys, usage accounting, failover or
metrics.

**Alternatives.**
1. Run the node agent on the desktop, so it registers and heartbeats like a
   phone.
2. A second provider in the client, pointing straight at the desktop server.
3. Operator-registered external nodes: the controller polls an
   OpenAI-compatible server and the gateway routes to it like a phone.

**Trade-offs.** (1) needs an agent build per desktop OS, would own the
desktop's inference server (which the user already runs and tunes in LM
Studio or Ollama) and pretends the desktop is a placement target. (2) needs
no code but splits the cluster: two keys, no shared usage, no failover
between desktop and phones, and the desktop cannot be a pool member. (3)
uses only the OpenAI API every such server already serves: `GET
/v1/models` for health and inventory, `/v1/chat/completions` and
`/v1/completions` for traffic. Its cost is a second kind of node the
controller does not manage: no heartbeat data (RAM, temperature), no class,
no placement.

**Decision.** (3).

- **Registration.** `PUT /admin/external/<name>` with `url`, optional
  write-only `api_key`, model allowlist, `max_concurrency` (default 1),
  `ctx_size` and a `speed_tps` hint; `GET` lists, `DELETE` removes. Names
  follow the alias rules (ADR-014) and share the alias namespace: a name
  cannot be a phone's alias or id, and a phone alias cannot be an external
  name. Configs, API keys and self-test results persist to
  `<state-dir>/external.json` (atomic, mode 0600). The key is never
  returned (`has_api_key` instead) and never logged. Drain flags stay in
  memory, like phones'.
- **Health.** Every 10 s, `GET {url}/v1/models` with the key as bearer and
  a 5 s timeout. A success makes the node `ACTIVE` with the listed model ids
  that pass the allowlist, in allowlist order; three consecutive failures
  make it `OFFLINE` and keep `last_error`. A new node starts `OFFLINE` until
  its first successful check, which the admin API runs synchronously so the
  response shows the real state. Transitions are logged and counted in
  `phoneborg_external_state_transitions_total{from,to}`;
  `phoneborg_external_up{node_id}` is the current state. A separate counter
  keeps `phoneborg_node_state_transitions_total` about phone lifecycles.
- **Identity.** Node id `ext:<name>`, alias `<name>`. Everything keyed by
  node id in the gateway (in-flight counts, cooldown, reaping, affinity
  pins, usage, per-node metrics) works unchanged.
- **Backends.** One `Backend` per (external node, model), so plain model
  ids route to it. Backends carry `External`, `APIKey` and
  `MaxConcurrency`. On forwarding, the gateway rewrites the body's `model`
  to the real id (a phone's llama-server ignores it, a multi-model server
  does not) and sets `Authorization: Bearer <api_key>` if a key is set. The
  gateway never copies client headers upstream, so a client's PhoneBorg key
  cannot leak. `node/<name>` goes to the first model (allowlist order, else
  server order): a node target names a machine, not a model.
- **Concurrency.** A node whose in-flight count (the gateway's existing
  counter, shared across its models) has reached `max_concurrency` is not a
  candidate. If no candidate is left, the request gets 503 `busy` rather
  than waiting: queueing would need a scheduler the gateway does not have.
  Desktop servers usually serialise requests anyway; 1 is the safe default.
- **Speed.** A self-test per model, on registration (first check) and when
  the model list gains a model without a measurement, and on demand
  (`POST /admin/external/<name>/selftest`): a fixed prompt with
  `max_tokens: 32`. llama.cpp `timings` are used when present; otherwise
  completion tokens over wall-clock time, which includes prompt processing
  and so underestimates. The result is `Backend.Speed` in tok/s, the same
  unit as phones' self-tests (ADR-010); `speed_tps` overrides it. A test
  that failed is not retried automatically.
- **Pools.** Members list one entry per external model. Node filters match
  `<name>` or `ext:<name>`; class filters never match (no RAM class); a
  pool's model filter now also applies per backend (`Target.Models`), since
  an external node serves several models.
- **Not managed.** The planner, `/admin/placement`, `/admin/nodes` and the
  device class counts see only phones. `/v1/models` lists external models
  (kind `model`) and `node/<name>` (kind `node`, `"external": true`).
  `/admin/stats` counts ACTIVE external nodes separately and includes their
  models.

**Not solved.** No queueing at the concurrency limit. `min_gen_tps` is
checked per (node, model) entry, but a pool target filters by a node set and
a model set: an external node's model that fails `min_gen_tps` is still
used through the pool if another eligible member serves the same model id.
No thermal data. The
wall-clock self-test includes prompt time and model loading (LM Studio JIT
loading can make the first test very slow). A server that lists models it
cannot load (LM Studio lists every downloaded model) routes requests to
them; use the allowlist. TLS verification uses the system roots; there is
no option for self-signed certificates.
## ADR-017: Ollama-compatible front and local-by-default access

**Problem.** Many local-LLM clients (Open WebUI, Continue, Raycast, some
opencode-adjacent tools) speak Ollama's HTTP API, not OpenAI's, and default
to `http://127.0.0.1:11434`. Separately, the gateway's inference endpoints
(`/v1/chat/completions`, `/v1/completions`, `/v1/models`) are open to anyone
who can reach port 18080 unless an operator explicitly turns on
`-api-keys-file` and enforces it: a phone cluster is easy to run without
keys during development, but "open unless configured" is the wrong default
once the controller listens on more than loopback (a laptop on a shared
network, a container with a published port).

**Alternatives, Ollama front.** (1) Tell users to point Ollama clients at
`/v1` and hope the client's OpenAI-compatible mode covers what they need.
(2) A separate proxy process translating Ollama to OpenAI. (3) Translate
inside the controller, reusing the existing gateway.

**Trade-offs.** (1) fails for clients that only speak Ollama or want
Ollama-specific fields (`options`, `raw`, NDJSON streaming). (2) adds a
process, a port and a second copy of auth/routing to keep in sync. (3) adds
one file's worth of translation but changes nothing about routing, failover,
affinity, usage or metrics, which the OpenAI path already gets right.

**Decision.** (3). `controller/gateway/ollama.go` adds `GET /api/version`,
`GET /api/tags`, `POST /api/show`, `GET /api/ps`, `POST /api/chat`,
`POST /api/generate` and 501 stubs for `/api/pull`, `/api/push`,
`/api/create`, `/api/delete`, `/api/copy`, `/api/embed`, `/api/embeddings`
(Ollama-shaped `{"error":"..."}"`; `pull`'s message points at `pbctl models
add`). `/api/chat` and `/api/generate` build an internal `*http.Request` for
`/v1/chat/completions` (or `/v1/completions` when `generate`'s `raw` is
true) and call the gateway's own `handleProxy` directly, so target
resolution (`auto`/`pool/`/`node/`), authentication, failover, affinity,
usage accounting and metrics are exactly the OpenAI path's, not a second
implementation. A `http.ResponseWriter` adapter
(`ollamaStreamAdapter`) sits in front of `handleProxy`: for a streaming
request it parses the upstream SSE line by line and writes+flushes one
Ollama NDJSON line per delta as it arrives, then a final `{"done":true,...}`
line with `total_duration`/`prompt_eval_count`/`eval_count`/`eval_duration`
computed from the same `usage`/`timings` the OpenAI path already parses; for
a non-streaming request a small buffering writer captures the OpenAI JSON
response once and translates it in one step. Either way, an error the
gateway would have sent as an OpenAI error envelope is re-shown in Ollama's
flat `{"error":"..."}` shape. `options.num_ctx` is accepted and ignored:
context size is a controller placement decision (ADR-011), not a per-request
one. `GET /api/tags`/`POST /api/show`/`GET /api/ps` read the same served-model
state as `GET /v1/models` (including the `auto`/`pool/`/`node/` virtual
models, ADR-014) through a new optional `Gateway.SetModelInfo` hook the
controller fills from the catalog, so size/digest/family/parameter
size/quantization are real when known and clearly-fake placeholders
(`"unknown"`, an all-zero digest) otherwise. These routes are served on the
main listener and, with the new `-ollama-listen` flag (default empty), on a
second address too, so a client hard-coded to Ollama's default port needs no
reconfiguration.

**Alternatives, access control.** (1) Leave it as is: open by default,
`-api-keys-file` plus `auth=keys` to close it, as today. (2) Require
`-api-keys-file` unconditionally. (3) A new `-gateway-access` flag with a
`local` default: peers in a trusted CIDR list need no key, everyone else
does.

**Trade-offs.** (1) is a footgun the moment the controller is reachable from
more than one machine (compose's published port, a shared dev box). (2)
breaks every existing single-user/localhost setup and demands key management
for a threat model (other processes on the same machine) that does not
apply there. (3) keeps localhost frictionless, the compose/Docker case safe
by trusting only the container bridge (an integrator concern, documented
rather than guessed at here), and gives multi-machine deployments a real
gate, at the cost of one more flag and a CIDR list to get right.

**Decision.** (3). `-gateway-access local|keys|open` (default `local`) and
`-trusted-cidrs` (default `127.0.0.0/8,::1/128`) are new controller flags.
`gateway.AccessControl` wraps `StaticKeys` as the gateway's `Authenticator`:
a request carrying a valid key is always attributed to its owner, in every
mode; otherwise `keys` rejects it (401), `open` serves it as `anonymous`
(today's behaviour, logged as a startup warning), and `local` serves it as
principal `"local"` when the peer is in `-trusted-cidrs`, else 403
`{"error":{"message":"PhoneBorg accepts requests from other machines only
with an API key (see -gateway-access)","type":"access_denied",
"code":"remote_requires_api_key"}}` (Ollama routes: the same message under
a flat `{"error":"..."}"`). The peer address is `r.RemoteAddr` unless it is
itself listed in the new `-trusted-proxies` (default empty), in which case
the first `X-Forwarded-For` address is used instead — so a stray or spoofed
header cannot claim trust unless the direct connection already comes from a
configured reverse proxy. The existing runtime switch (`PUT /admin/gateway
{"auth_mode":"keys"}`, `pbctl gateway set auth=keys`, ADR-008) still works
and takes precedence over `-gateway-access`: once `StaticKeys.Enforce()` has
been called, by any path, every mode behaves like `keys`, so the two
controls cannot disagree. `GET /admin/gateway` and `pbctl gateway` gain an
additive `access` field showing the effective mode. Access control wraps
only the gateway's `Authenticator`, so it applies to exactly
`/v1/chat/completions`, `/v1/completions`, `/v1/models` and `/api/*` — never
the node protocol, `/metrics`, `/healthz`, `/ui/` or `/admin/`, which keep
their existing trust models (ADR-002, ADR-008, ADR-013) untouched. Denials
are counted in the existing `phoneborg_gateway_rejected_total
{reason="remote_requires_api_key"}`.

**Docker note.** A container sees connections from the compose bridge
network's gateway address, not `127.0.0.1`, so `-trusted-cidrs` needs that
bridge subnet (or the compose file must publish the controller's port on
`127.0.0.1` only and rely on the host's own loopback trust) — see
[OPERATIONS.md](OPERATIONS.md#access-control) for the concrete setting; the
compose file itself is for the integrator to update.

**Trade-offs.** `local` mode is still only IP-based trust, not
authentication of the machine itself: anything that can spoof or share a
trusted address (a compromised host on the same LAN segment placed in
`-trusted-cidrs` by mistake) is trusted too. That is the same trust model
`adb reverse`/`adb forward` already give the node protocol (ADR-002,
ADR-003); this ADR does not change it, only extends a version of it,
opt-out-able via `-gateway-access open`, to the inference endpoints.
`ollamaStreamAdapter` buffers only the current SSE line (not the whole
response) for translation, so memory use for a streaming reply stays
proportional to one line, not the full generation — the same property the
existing OpenAI path already has via `forward`'s chunked copy.

**Not solved.** Ollama's request-side `tool_calls` on history messages (a
past assistant turn) are forwarded unchanged rather than reshaped between
Ollama's and OpenAI's slightly different shapes, since messages are passed
through verbatim; only the top-level `tools` definitions (already
OpenAI-shaped in Ollama) and the response's `tool_calls` are actively
mapped. `format` as a JSON Schema object (not the literal string `"json"`)
is not translated. `-trusted-proxies` trusts every request from a listed
peer equally; there is no per-header allowlist or chained-proxy parsing
beyond the first `X-Forwarded-For` address.

## ADR-018: "Chat test" in the web panel

**Problem.** Operators had no way to try inference from the panel itself:
proving a model works meant a terminal and curl or `pbctl`. The obvious
implementation, the panel calling `POST /v1/chat/completions` with the admin
token, does not work in the production deployment: with `-gateway-access
local` (ADR-017's default), a remote browser is neither a trusted peer nor
holder of an API key, and the admin token is deliberately not a gateway
credential (mixing the two would let anything that can reach the panel also
mint itself unlimited inference access, and `/v1/models` already shows this
gap — the panel's own Overview falls back to "not reachable" for it under
`-gateway-access local`). A concrete design question follows: how does a
browser holding only the admin token reach the gateway's routing, failover
and streaming, without weakening `/v1/*`'s access control?

**Alternatives.**
1. Accept the admin token as an alternative on `/v1/*`.
2. A separate, minimal inference path under `/admin/` with its own
   request/response handling, independent of the gateway.
3. A new endpoint under `/admin/` (admin-token-authenticated, like every
   other panel call) that calls into the gateway's existing proxying
   directly, the same way `controller/gateway/ollama.go` calls
   `handleProxy` for `/api/chat` (ADR-017), so routing, failover, affinity,
   usage and metrics are not duplicated.

**Trade-offs.** (1) works but blurs two different trust boundaries: the
admin token would become a second, unauditable way to spend inference
capacity from anywhere the panel is reachable, exactly what ADR-017 set out
to avoid. (2) avoids that but re-implements target resolution, candidate
selection, retries and usage/metrics recording a second time, which is what
ADR-006/ADR-010/ADR-014's gateway already does correctly. (3) keeps exactly
one implementation of gateway behaviour and adds only a thin admin route,
at the cost of one exported method the gateway did not previously need
(the ADR-017 Ollama front could stay in-package and call `handleProxy`
directly; the admin API is a different Go package, so it needs an exported
entry point that skips re-authenticating an admin-already-checked request).

**Decision.** (3).

- **`POST /admin/chat/completions`** (`controller/chat_admin.go`) is
  authenticated like every other admin route (`adminAuth`, the admin
  token). It sets the request path to `/v1/chat/completions` and calls the
  gateway's new exported `Gateway.ServeChat(w, r, principal)`
  (`controller/gateway/proxy.go`), which is `handleProxy`'s body factored
  out from `Authenticate`: routing (`auto`/`pool/`/`node/`/model), context
  checks, failover, affinity, usage recording and every gateway metric run
  exactly as for `/v1/chat/completions`, with the caller supplying the
  principal instead of the request being authenticated a second time.
  Usage and `phoneborg_gateway_requests_total`/`..._tokens_total` attribute
  this traffic to a distinct principal, `panel` (`PanelPrincipal`), so it is
  never folded into `anonymous`, `local` or a real API key's numbers.
  Streaming passes through unchanged (the panel always sends
  `"stream":true`); `adminAuth`'s status-tracking `ResponseWriter` wrapper
  gained an `Unwrap() http.ResponseWriter` method (the standard Go 1.20+
  `http.ResponseController` hook) so `forward`'s per-chunk `Flush()` still
  reaches the real connection through it.
- **`GET /admin/chat/models`** returns the same list `GET /v1/models`
  would (`Gateway.ModelEntries()`, exported from the previously unexported
  `modelEntries`, shared with `GET /api/tags`): served models, then `auto`,
  pools and aliased nodes (ADR-014). The panel's model selector uses this
  instead of `/v1/models` for the same reason the chat endpoint does not
  use `/v1/chat/completions`: the admin token is not necessarily a valid
  gateway credential.
- **Panel.** A new "Chat" view (`controller/ui/static/js/views/chat.js`):
  model selector, optional system prompt, a message box and streamed
  output via the Fetch API's `ReadableStream` (parsing the same SSE the
  gateway already forwards), a **Clear** button, and, after each reply, the
  serving node (`X-PhoneBorg-Node`, resolved to its alias via
  `GET /admin/nodes`), token counts and tok/s (from the response's
  `usage`/`timings`, whichever the backend sent) and latency measured in
  the browser. Conversation history lives only in the page (a JS array),
  never persisted; it is lost on refresh or leaving the tab, like every
  other view's in-progress form state (ADR-013).

**Not solved.** The chat endpoint is not counted separately from other
admin actions beyond the existing `phoneborg_admin_actions_total{action=
"chat_completions"}`; a busy panel session looks like any other admin
traffic there; the gateway's own per-principal metrics are what show it is
`panel` traffic. There is no server-side cap on how much a panel session can
generate beyond the gateway's normal `-upstream-timeout` and node
concurrency limits.

## ADR-019: USB device auto-detection in the panel and conflict-free provisioning

**Problem.** The controller only knows about nodes that have registered: a
phone that is `unauthorized`, `offline`, mid-provisioning or failed is
invisible to it, so an operator has to watch `pcprov`'s own logs on the adb
host to see why a phone never showed up. There was also no way to pause
automatic provisioning from the panel (only by stopping `pcprov watch`
itself, which also stops adb-link healing), and no per-device conflict
handling: `pcprov watch` retried a failing phone every 3 s forever, two
`pcprov` processes could in principle drive the same adb server at once, and
a duplicate or junk serial (seen on some cheap/misconfigured USB
descriptors) was provisioned like any other device.

**Alternatives.**
1. Leave `pcprov` a purely local CLI tool; operators read its logs.
2. Have the controller run `adb` itself (poll `adb devices`, provision) and
   drop `pcprov` as a separate process.
3. `pcprov watch` keeps owning adb (ADR-001/ADR-003's model: only the adb
   host's `pcprov` touches phones) and reports its device view to a new
   controller admin endpoint; the controller stays adb-free and only
   displays/steers what pcprov tells it.

**Trade-offs.** (1) needs a terminal on the adb host for anything beyond
"is a phone eventually ACTIVE". (2) collapses two processes into one, but
means the controller process needs `adb` on its `PATH` and both provisioning
and the gateway/admin API now share one process's failure domain, and two
controllers (e.g. an accidental second instance) would then need the same
adb exclusivity problem solved a second way. (3) keeps the existing
separation and adds one small, opt-in reporting protocol, at the cost of the
controller's view of devices being only as fresh as `pcprov`'s last report.

**Decision.** (3).

- **Reporting protocol** (`proto/devices.go`, alongside the node protocol's
  wire types per ADR-002's convention: flat, JSON, no new dependency for
  `pcprov`, which does not otherwise import `controller`).
  `POST /admin/devices/report` (`proto.PathDevicesReport`) takes a
  `DeviceReport{Host, Devices, Acked}`: `Host` identifies the reporting
  `pcprov` (its hostname), `Devices` is pcprov's **whole** current view
  (replacing, not merging, the controller's previous view from that host),
  and `Acked` names serials whose queued command (below) pcprov executed
  this cycle. Each `DeviceStatus` carries the serial, raw adb state, model,
  a lifecycle `Status` (`new`, `waiting-authorization`, `provisioning` with
  `Step`, `provisioned`, `failed` with `Error`/`Hint`, or `gone`, reported
  once and then dropped) and first/last-seen timestamps. The response,
  `DeviceReportResponse{AutoProvision, Commands}`, carries the setting below
  and any pending one-shot commands for pcprov's devices.
- **`pcprov` flags**: `-controller-url` (default
  `http://127.0.0.1:18080`) and `-admin-token-file` (default empty).
  Reporting is enabled only when a token file is given, so every existing
  `pcprov watch`/`provision` invocation keeps working unchanged. `watch`
  reports on every state change and at least every 15 s
  (`reportInterval`), matching the existing 3 s poll / 15 s adb-link-heal
  cadence already in the loop.
- **Controller state** (`controller/devices.go`): an in-memory map keyed by
  reporting host, holding each host's last device list and its last-report
  time, plus one `auto_provision` bool and a set of pending per-serial
  commands, all behind one mutex. `GET /admin/devices` lists every device
  across every host, each with the linked node id/alias (best-effort: the
  node id equals the phone's `ro.serialno`, ADR-001, so a device and a node
  are linked when their ids match) and a `stale` flag once its host has not
  reported for `DeviceStaleAfter` (45 s, three report intervals) — the panel
  and `pbctl devices` show this as "not reporting" rather than silently
  showing an outdated device. `PUT /admin/devices {"auto_provision":bool}`
  changes the setting, persisted to `<state-dir>/devices.json` (version,
  atomic write, mode 0600) exactly like `routing.json`/`placement.json`;
  default `true` (today's behaviour) when no state dir or no file yet.
  `POST /admin/devices/{serial}/provision` queues a command, delivered in
  the next report response and cleared only once pcprov acknowledges it —
  asking again before that is a no-op (idempotent). One command covers both
  the panel's **Provision** (a device auto-provisioning left alone) and
  **Retry** (a failed one) actions, since to pcprov both mean "attempt this
  serial now, regardless of `auto_provision` or backoff".
- **`auto_provision=false`** stops `pcprov watch` from provisioning *new*
  devices on its own; it keeps polling `adb devices`, reporting every
  device's status and healing already-provisioned devices' adb links every
  15 s exactly as before (ADR unrelated to healing). A queued
  provision/retry command bypasses the setting, and even bypasses an
  already-successful provision in the same run (e.g. to push a rebuilt
  agent) — the only thing that overrides *nothing* is the duplicate/
  placeholder guard below.
- **Backoff and reset** (`provisioner/devices.go`, pure and unit-tested):
  a failed attempt waits 30 s before the next automatic retry, doubling on
  each further failure up to a 10 min cap, instead of retrying every 3 s
  tick. The device disappearing (unplugged) and reappearing, or an explicit
  operator retry, resets it. Watch is single-threaded — one `Provision`
  call in flight at a time, in the same goroutine that also does the adb
  polling and reporting — so a serial can never be provisioned twice
  concurrently within one process; a `deviceRecord.provisioned` flag stops
  it from being auto-retried at all once it has succeeded.
- **Duplicate/placeholder serials**: `pcprov` never provisions two devices
  that `adb devices` currently reports with the same serial (`adb -s
  <serial>` would be ambiguous), or a serial from a small known-junk list
  (e.g. `0123456789ABCDEF`, seen on some misconfigured USB descriptors).
  Both are reported `failed` with a clear error and hint, and neither is
  provisioned even by an explicit "provision now"/"retry" command.
- **Exclusivity**: `pcprov watch`/`provision` take an exclusive,
  non-blocking `flock` (`syscall.Flock`, `provisioner/lock_unix.go`) on a
  file in `os.TempDir()` (never a per-session directory like
  `$XDG_RUNTIME_DIR`, which a systemd service and a login shell see
  differently), named by the
  adb server port (`$ANDROID_ADB_SERVER_PORT`, default 5037 — adb's own
  notion of "one server per port", not the controller's port, since two
  different adb servers on one host never conflict). A second instance
  fails immediately with a message naming the first one's PID, read back
  from the lock file. Read-only commands (`devices`, `status`, `stop`,
  `slim`/`unslim`, `heal`) take no lock. `provisioner/lock_other.go`
  (`!linux && !darwin`) is a no-op, so the package still builds elsewhere;
  ADR-019 targets the Linux/macOS hosts pcprov already runs on.
- **Port collision check** (requested, not assumed): `adb forward tcp:0
  tcp:<device port>` (`provisioner/adb.go`) already lets adb/the OS assign a
  genuinely free ephemeral host port per call, and Watch's single-threaded
  loop means these calls are never concurrent within one process; the new
  lock now also rules out a second process racing the same adb server. No
  collision was found, and no change was made to port selection beyond the
  lock itself.
- **Panel and pbctl.** A new **Devices** view (`controller/ui/static/js/
  views/devices.js`), polled on the same 5 s live-view cycle as every other
  view (ADR-013): status badges, hint/error, linked node, reporting host
  (with a "not reporting" badge when stale), an auto-provision toggle and
  per-device **Provision**/**Retry** buttons. `pbctl devices`, `pbctl
  devices auto on|off` and `pbctl devices provision <serial>` are thin
  clients of the same three endpoints, in the existing `pbctl` style (its
  types come from `controller`, ADR-008).

**Trade-offs.** The controller's device view is only as fresh as the last
report; a `pcprov` that dies without reporting "gone" leaves stale entries
visible until `DeviceStaleAfter` marks the whole host stale (not per
device). A "gone" device is visible for exactly one report cycle and then
disappears with no history. The node/device link is best-effort string
matching, not a real identifier carried through both protocols; a node
started with `node-agent -node-id` overriding the default breaks it. Nothing
stops two different operators from both holding the admin token and issuing
conflicting provision requests — the same single-admin-role limitation
ADR-008 already accepted.

**Not solved.** Multiple `pcprov` hosts can report to one controller (kept
distinctly by `Host`), but this is not a scenario the project otherwise
documents or tests end-to-end. There is no rate limit on `POST
/admin/devices/report` beyond it being just another admin action; a
misbehaving reporter could still be throttled only by revoking its token.

## ADR-020: Super Borg mode — one model, an orchestrator delegating to phones

**Problem.** With four phones of very different strength, the operator
wants the cluster to behave as one assistant: a stronger phone (the
OnePlus, 12 GB, Qwen3-8B) reasons about the request and hands parts of it
to the smaller models, choosing who does what, instead of the client
picking `auto`, a pool or a node. The phones run llama-server, which knows
nothing about the other phones; something has to run the plan–delegate–
answer loop.

**Alternatives.**
1. A separate agent process (Python or Go) on the host that calls the
   gateway like any client.
2. A request classifier that only routes each request to one phone
   (docs/TODO.md item 2, "Laya").
3. The loop inside the gateway, as a cluster-wide mode: the orchestrator
   model gets a `delegate` tool; the gateway executes its calls on the
   other nodes through the existing `forward` path.

**Trade-offs.** (1) keeps the gateway simple but duplicates node selection,
health, failover, usage and metrics, and still needs a switch so clients
cannot bypass it. (2) is cheap per request but never splits work, so it
cannot use the phones in parallel. (3) reuses `forward` for every
orchestrator and worker call (in-flight accounting, reaping of lost nodes,
per-node usage and metrics), sees the live backend set and speeds, and the
mode switch is natural at the one place requests enter. Its cost is a
second request path in `ServeChat` and an SSE parser for the
orchestrator's stream.

**Decision.** (3).

- **Mode** (`controller/superborg.go`): `{enabled, orchestrator,
  thinking}`, stored in `routing.json`, served as `GET/PUT
  /admin/superborg`, `pbctl superborg`, and a card in the panel's Pools
  view. While enabled, `ModelEntries` lists only `superborg`, so
  `/v1/models`, `/api/tags` and the panel all show one model.
- **Loop** (`controller/gateway/superborg.go`): the orchestrator gets a
  system prompt with the worker roster (alias, model, catalog params and
  tags, measured tok/s; sorted and free of volatile data so llama-server's
  prefix cache keeps it) merged into the client's system message, and one
  tool, `delegate({tasks:[{worker, task}]})`, with the worker names as an
  enum. Its request is always streamed; content and reasoning deltas are
  relayed to the client as they arrive, tool-call deltas are accumulated.
  Delegated tasks run in parallel as non-streaming requests with
  `max_tokens` 512 and thinking off; results go back as `tool` messages.
  At most 2 rounds; the round after is sent without tools.
- **Prompt rules** (from the first live runs, where a 4B orchestrator sent
  a translation of a not-yet-written poem in the same round and gave Polish
  writing to a 0.5B model): tasks in one call run at the same time, so no
  task may need another's result; models under 2B params get only very
  simple tasks; worker answers must be checked. The rules use parameter
  counts from the catalog, not model names.
- **Choice of nodes**: no model names are hard-coded (AGENTS.md). The
  orchestrator is the configured alias/id, else the ready node serving the
  largest catalog model; the workers are all other ready, not hot nodes.
- **Failure handling**: an orchestrator failing before any output is
  replaced by the next-largest node; a failed worker task is retried once
  on another worker, then reported to the orchestrator as an error text;
  an error after output started ends the stream with a bracketed message.
- **Idle timeout for the orchestrator**: its calls use `-upstream-timeout`
  as the longest silence between response chunks rather than a total limit.
  The first live run with thinking on was cut off after exactly 600 s
  while Qwen3-8B on the OnePlus was still generating the final answer.
  Workers keep the total limit; their output is capped at 512 tokens.
- **Bypass**: `/v1/completions` and requests with client `tools` go to the
  orchestrator unchanged, so agent clients keep their own tool loop.

**Consequences.** Delegation multiplies latency on phones: plan, parallel
subtasks, then synthesis of their results, each with prompt processing at
roughly 10–20 tok/s. Super Borg pays off for requests that split into
independent parts; simple questions cost about the same as asking the
orchestrator directly. Thinking is off by default for that reason. While the
mode is on, pools, node targets and `auto` are unavailable to clients by
design.

## ADR-021: Super Borg jobs — long work with server-side state

**Problem.** Super Borg chat (ADR-020) broke down on a real multi-step
request: a story for a 5-year-old in 20 chapters of about 3 minutes each,
planned and written together with the workers. Over several chat turns the
orchestrator (Qwen3-8B on a phone) claimed chapters that did not exist and
took 16–42 minutes per turn before the first word. Causes:

1. Worker results existed only inside one chat request; the next request
   carried only the orchestrator's earlier prose, so it invented progress.
2. The chat history grew to 5–10k tokens, re-processed every turn at about
   12–17 tok/s on the phone.
3. Two delegation rounds per request cannot cover 20 chapters.
4. The finished story (~15k tokens) does not fit the orchestrator's 16k
   context at all, so it cannot be one answer.

**Alternatives.**
1. Raise the round limit and keep everything in the chat request.
2. Persist the delegation messages per conversation and resend them.
3. A background job with server-side state — a task list and a workspace of
   named documents — driven by stateless orchestrator steps, as agent
   harnesses (Claude Code, Codex, Hermes) do with todo lists, files and
   fresh-context subagents.

**Trade-offs.** (1) makes the context problem worse and ties hours of work
to one open browser tab. (2) fixes the forgetting but not the growth: every
chapter would flow through the orchestrator's context. (3) needs a runner,
storage, an API and a view, but keeps the orchestrator's prompt small and
constant, never loses a result, survives restarts and closed tabs, and
assembles a result of any size outside the model.

**Decision.** (3).

- **State** (`controller/gateway/jobs.go`): goal, later user messages,
  tasks `{title, doc, status}`, documents `{name, text, author}`, event log,
  last 6 step results; one JSON file per job in `<state-dir>/jobs/`, saved
  after every step. Running jobs are re-queued on startup.
- **Steps**: each step is a fresh two-message request (rules + roster; a
  compact view of the job) with `tool_choice: required`, so a small model
  must act through a tool instead of narrating. Tools: `plan_tasks`,
  `write_doc`, `read_doc`, `delegate` (with `context` documents injected
  into the worker prompt and `save_as` for the result), `ask_user`,
  `finish`. A task is done when its document is written.
- **Limits**: one job at a time; 80 steps per run, then it waits for a
  message, and so do 3 steps in a row without a tool call (seen live when
  the OnePlus was unplugged and the fallback orchestrator, Gemma 3n, wrote
  "tool_code" as text instead of calling tools); 5 failed steps in a row
  fail it; worker output 1200 tokens,
  injected context 12 kB, orchestrator output 3000 tokens. Orchestrator and
  worker calls stream with the idle timeout of ADR-020.
- **Who and how many**: the gateway assigns delegated tasks to the fastest
  free workers; the orchestrator only says what to do. Left to Qwen3-8B,
  17 of 19 chapters of a story went to the first worker in its list (the
  slowest phone). Each step lists as many next tasks as there are workers,
  so parts are written in parallel, with the outline and part list as
  shared context; a task that needs a planned document that does not exist
  yet is refused.
- **Entry points**: `POST /admin/jobs` and the rest of `/admin/jobs/...`,
  `pbctl jobs`, the panel's Jobs view, and a `start_job` tool in the Super
  Borg chat, which creates a job from the user's last message verbatim.
- **Result**: the documents `finish` names (else the task documents in
  order), joined as Markdown by the controller.

**Consequences.** Long work becomes slow but reliable and inspectable: every
document can be read, redone or steered by a message while the job runs.
The quality of planning and writing is still that of the small models; the
mechanism only stops it from falling apart. Jobs compete with chat traffic
for the same phones, and only one runs at a time.

## ADR-022: Super Borg as a pool routing instead of a cluster-wide mode

**Problem.** ADR-020 made Super Borg a switch for the whole cluster: while
on, `/v1/models` listed only `superborg` and every request went to the
orchestrator. That does not scale to a large fleet, where some phones
should form a Super Borg and others serve agents such as opencode, and it
broke those agents outright: `pbctl opencode sync` reads `/v1/models` and
would have dropped every pool and `node/<alias>` agent, and opencode
subagents aimed at `pool/fast` all landed on the orchestrator.

**Alternatives.**
1. Keep the mode and add exceptions (explicit targets bypass it).
2. Make Super Borg a routing of pools: `routing: "superborg"`, with the
   orchestrator and thinking settings on the pool.

**Trade-offs.** (1) keeps one switch but needs rules for which targets
bypass it and still hides models from `/v1/models` while on. (2) removes the
global state altogether: a fleet is split by pool membership, every model
stays listed, and several Super Borgs can coexist. Its cost is a migration
of the old setting and a pool to choose for jobs.

**Decision.** (2).

- `Pool` gains `routing: "superborg"`, `orchestrator` and `thinking`; the
  pools API reports `active_orchestrator`. The gateway's `Target` carries
  the settings, and a chat request to such a pool runs the ADR-020 loop
  with the orchestrator and workers taken from the pool's eligible members
  only. Raw completions and requests with client tools go to the
  orchestrator member.
- `/admin/superborg`, `pbctl superborg` and the panel's Super Borg card are
  removed. On startup an old `superborg` setting in `routing.json` becomes a
  pool named `superborg` over every node with the same orchestrator and
  thinking, and is no longer written.
- Jobs (ADR-021) run on a Super Borg pool: `pool` in `POST /admin/jobs`,
  `pbctl jobs new pool=`, a choice in the Jobs view; the default is the
  first Super Borg pool by name, and the whole cluster if there is none.
  `start_job` from a pool's chat uses that pool. A job whose pool is no
  longer a Super Borg pool waits with a message instead of retrying.

## ADR-023: Route tool requests to tool-capable models

**Problem.** Agent clients (opencode with tools) send `tools`. A pool such
as `pool/fast` mixes models that call tools well (Qwen3) with ones that do
not: Gemma 3n answers with the text `tool_code …` instead of a tool call,
which breaks the agent. The same failure made a Super Borg job loop when
Gemma became the automatic orchestrator.

**Decision.** Tool capability is catalog data, not code: models that call
tools reliably carry the tag `tools` (`pbctl models tag`). The gateway keeps
only tool-capable candidates for requests that offer tools, inside the
named target; with none it falls back to every candidate, so untagged
clusters are unaffected, and `node/<alias>` is never filtered. The Super
Borg orchestrator's automatic choice ranks tool-capable models first.
Counted in `phoneborg_gateway_tool_requests_total{result}`.

**Consequences.** Model names stay out of the code (AGENTS.md); the operator
must tag new models. A wrong tag shows up as a failing agent, a missing one
as a `fallback` count.

## ADR-024: Separate limits for prompt processing and generation

**Problem.** `-upstream-timeout` bounded a whole proxied request. On phones
two different waits hide in it: prompt processing (no output at all; an
opencode build-agent prompt of ~11.7k tokens takes 10–15 minutes at
12–17 tok/s) and generation (steady output that can run long, e.g. a
thinking model). Any single limit either cuts off long agent prompts or
lets a stuck node hang a client for the same long time once it streams.
ADR-020 had already turned the limit into an idle limit for Super Borg
orchestrator calls only.

**Decision.** Every forwarded call has two limits: until the node's first
response byte, `-first-token-timeout` (default 30 min, `first_token_timeout`
in `PUT /admin/gateway`); after it, `-upstream-timeout` is the longest
silence between chunks, reset on every chunk. A first-token timeout before
any byte reached the client is a retryable failure, so the request may
still go to another node. 0 means "same as the upstream timeout", so code
and tests that set only one limit keep their behaviour. The special case
for Super Borg orchestrator calls is gone; they follow the same rule.

**Consequences.** A non-streaming response arrives in one piece, so for it
the first-token timeout bounds the whole request. A streaming node is never
stopped while it produces tokens; generation is bounded by its context
size and the client's own cancel.

## ADR-025: The cluster as MCP tools (`pbctl mcp`)

**Problem.** Super Borg jobs (ADR-021) are reachable from the panel and
`pbctl`, but an agent that a user works with, such as opencode on a strong
API model, cannot hand work to the phones except as chat completions on a
provider, which offers no way to start, steer or collect long jobs.

**Alternatives.**
1. An MCP endpoint on the controller (Streamable HTTP).
2. A local MCP server over stdio in `pbctl`, talking to the controller's
   existing APIs.

**Trade-offs.** (1) needs a new authenticated HTTP surface, sessions and
SSE on the controller. (2) reuses the admin API, its token and the
gateway, adds no network surface, and is how opencode and most MCP clients
run tools anyway; its cost is that every client host needs `pbctl` and the
admin token in its environment.

**Decision.** (2). `pbctl mcp` speaks MCP (JSON-RPC 2.0, newline-delimited;
revisions 2025-06-18, 2025-03-26 and 2024-11-05) with tools
`cluster_status`, `ask_cluster`, `job_start`, `job_status`, `job_message`,
`job_result` and `jobs_list`, later `cluster_map` (parallel independent
tasks), `cluster_vote` (several phones judge one question, with an
optional threshold) and `job_wait` (blocks until a job changes, so an agent
can follow it in a loop). Tool failures are tool results with
`isError`, not protocol errors, so the model can read them.
`pbctl opencode init` writes `mcp.phoneborg` with the absolute pbctl path
and `{env:PHONEBORG_ADMIN_TOKEN}`, never the token itself; `sync` replaces
only the provider block now, so `mcp` and `small_model` survive. It also
writes `.opencode/phoneborg.md`, listed under `instructions`, which tells
the main agent when to use the subagents, `ask_cluster` and jobs, so it
delegates without being reminded; a hand-edited copy is kept. The admin
token gives the agent the admin API's power over jobs; a scoped token is
future work.

## ADR-026: Named admin tokens

**Problem.** ADR-008 has one admin token. With the panel open on a laptop
and `pbctl` on the server sharing it, the audit log could only say
`"principal":"admin"` and an address: a placement change that cleared every
policy could not be told apart from a script on the server by anything but
the IP.

**Alternatives.**
1. Several tokens in `-admin-token-file`, one per line with a name.
2. Keep that file as it is and add named tokens, managed through the admin
   API, in a separate hashed file.

**Trade-offs.** (1) changes a file that `pcprov` and scripts read as "the
first line is the token", and stores the secrets in plain text. (2) leaves
that contract alone, stores only hashes like API keys, and lets an operator
issue and revoke tokens from the panel or `pbctl` without editing files.

**Decision.** (2). The `-admin-token-file` token is named `admin`. Named
tokens (`^[a-z0-9][a-z0-9-]{0,31}$`) live in `-admin-tokens-file`
(default `<state-dir>/admin-tokens`, else memory only) as
`<name> sha256:<hex> <created>` lines, created with
`POST /admin/tokens` (the token is returned once), listed with
`GET /admin/tokens` and revoked with `DELETE /admin/tokens/{name}`; `admin`
cannot be revoked there. Authentication compares the request's token with
every hash in constant time and records the token's name as the request's
principal, which the audit log and the debug log of reads show. `pbctl
admin-tokens` and the panel's API keys view manage them. All tokens keep
the single admin role; scopes are future work.

## ADR-027: Pool status, on/off switch and no conflicting pools

**Problem.** Operators read pools as modes that are "on" or "off". When
every phone was switched to a model a pool did not allow, `pool/smart` had
no eligible node and opencode failed with "no eligible ready node", with
nothing in the panel saying why. Pools could also be defined so that they
can never both be served: two pools naming the same phone but allowing no
common model, while a phone serves one model at a time.

**Decision.**
- Every pool reports a computed `status`: `ready` (with the number of
  eligible nodes), `no ready node` (with the most common member reason,
  e.g. no node serving an allowed model) or `disabled`.
- `disabled` on a pool switches it off without deleting it: it is left out
  of `/v1/models`, its requests get 503 `pool_disabled` (counted in
  `phoneborg_gateway_rejected_total`), its target matches no backend (so
  prewarm and Super Borg do nothing), and jobs on it wait; the default job
  pool skips disabled pools.
- Saving or enabling a pool is refused with 409 when it names a node that
  another enabled pool also names and the two model allow-lists are both
  set and disjoint. Pools that take any node, or any model, never conflict:
  they adapt to what the phones serve. Placement stays the only thing that
  chooses models (ADR-011); pools still do not load models.

## ADR-028: Multi-model benchmark (MMB)

**Problem.** Choosing a model per phone relied on the planner's predicted
speed (ADR-015) and one self-test per loaded model. Operators asked for real,
comparable numbers: every model that fits a phone, loaded and timed the same
way, with time to first token, prompt and generation speed, and the answer
time after a first prompt (the prompt cache), for one phone or several,
one by one or at the same time.

**Decision.**
- A run (`POST /admin/mmb`, `pbctl mmb run`, the panel's MMB view) names
  phones or a pool, optional models (default: every ready catalog model
  that fits each phone by class and RAM budget), and whether the phones run
  in parallel. One run at a time.
- Per phone: drain it, then for each model set a **benchmark override**, an
  in-memory forced pin applied by `Replan` in front of the placement spec
  (reason `benchmark`), wait until the phone serves the model (load time),
  and send three requests straight to it through `Gateway.Probe`, which
  reaches a drained backend and times the first streamed token: a
  ~500-token prompt (cold), the same prefix again (warm), and a one-line
  question; 128 fixed tokens (`ignore_eos`) and thinking off for comparable
  generation speeds. Afterwards the override is cleared and the drain state
  restored; cancelling or a restart does the same.
- Runs are stored per run in `<state-dir>/mmb/`; an unreadable file is
  skipped, not fatal. Results are counted in `phoneborg_mmb_results_total`.

**Consequences.** A benchmark takes phones out of service for minutes per
model (downloads included) and replaces their models meanwhile; pools that
need those phones lose them for that time. Numbers depend on temperature
(ADR-010): a hot phone throttles, which the table does not correct for.

## ADR-029: External orchestrators

**Problem.** The Super Borg orchestrator (ADR-020, ADR-022) is the
bottleneck: Qwen3-8B on a phone processes prompts at about 6 tok/s and
plans weakly. Strong models are available elsewhere: Ollama or oMLX on a
desktop, LM Studio, or a hosted API such as DeepSeek. External nodes
(ADR-016) can already serve any OpenAI-compatible model, but every one of
them takes part in normal routing, so a paid API would also get `auto`
traffic, pool requests and Super Borg worker tasks.

**Decision.** An external node gets an optional `role`. With
`role: "orchestrator"` its backends are marked `OrchestratorOnly`: they
match no target except `node/<name>` (so not `auto`, model ids or pools),
are left out of `/v1/models`, are never a Super Borg worker and are never
chosen as orchestrator automatically. A Super Borg pool whose
`orchestrator` names such a node uses it even though it is not a member;
if it is down, the pool falls back to its own best member. Pools list it
with the reason `orchestrator only`. `pbctl external add ... role=...`
sets it; it is stored in `external.json`.

**Consequences.** Any OpenAI-compatible server with reliable tool calling
can plan for the phones without being exposed to other traffic. Hosted
APIs still see the controller's health polls and one short self-test per
model.

## ADR-030: Firmware check

**Problem.** Old phones often run outdated firmware: the POCO F3 of the
reference cluster had a 31-month-old security patch and stock MIUI, which
also leaves less RAM for models than LineageOS. Finding out what is newer
means looking up each phone by hand on different vendor sites.

**Decision.**
- The agent reports, from `getprop`, the brand, the device codename, the
  build id, display id and incremental version, the security patch date,
  the LineageOS version and the bootloader state (`ro.boot.flash.locked`,
  else the verified boot state). This needs no network and is always shown.
- With `-firmware-check` (off by default) the controller fetches, once a
  day and an hour after a new phone appears: the LineageOS build API per
  codename, Google's full OTA image page (Pixels) and the
  XiaomiFirmwareUpdater tracker's `latest.yml` (Xiaomi, Redmi, POCO; a
  community source). Only the sources the connected phones need are
  fetched. Pixel builds match by codename, skipping carrier builds;
  Xiaomi builds match by codename and the device-and-region part of the
  version (`KHMIXM` in `V816.0.5.0.TKHMIXM`), Stable branch only.
- `GET /admin/firmware` reports, per phone, what runs, the patch age, the
  bootloader, `latest` and a list of updates of three kinds: `update` (a
  newer build of the same firmware), `upgrade` (a newer LineageOS version)
  and `alternative` (LineageOS for a phone on stock firmware).
  `POST /admin/firmware/check` checks now. The panel's Nodes view shows a
  Firmware column and a section in the node details.
- Samsung's FOTA server refused requests and OnePlus has no public source,
  so those brands show only the patch age.

**Consequences.** Device codenames leave the network, to Google, LineageOS
and GitHub, which is why the online part is opt-in. Scraped sources (the
Google page, the community tracker) can change format; a failing source
keeps its last data and is reported in `errors`. Installing anything stays
manual: flashing needs the phone in hand, and an unlocked bootloader
wipes it.

## ADR-031: Grafana in the panel

**Problem.** Metrics live in Grafana on its own port, the panel on the
controller's: two places, two logins, and Grafana's port must be reachable
from every browser. The panel's CSP only allows content from its own origin,
so a plain iframe of Grafana is blocked.

**Decision.**
- With `-grafana-url` the controller reverse-proxies Grafana under
  `/grafana/` on its own port, as Rancher does. Grafana is configured to
  serve from that sub-path (`serve_from_sub_path`, a `root_url` ending in
  `/grafana/`) and to allow embedding. The proxy keeps the request's Host,
  which Grafana compares with the Origin of POST requests.
- An iframe cannot send the panel's bearer token, so the panel trades it
  (`POST /admin/grafana/session`) for an HttpOnly, SameSite=Strict cookie
  scoped to `/grafana/`, valid 12 hours and signed with a key made at start.
  `/grafana/` without a valid cookie answers 401. The proxy strips that
  cookie and any `Authorization` header before Grafana sees the request.
  This is the only admin call made with cookies; all others still omit them.
- The panel gets a **Dashboards** view (the PhoneBorg dashboard in kiosk
  mode, with time-range buttons), the per-node tokens-per-minute panel in
  the node details, and the top-bar Grafana link pointing to `/grafana/`.

**Consequences.** Everything is reachable through the controller's port.
Grafana decides what the embedded user may do: with anonymous Viewer access
(the reference VM and the compose stack) the panel shows dashboards
read-only; editing still needs a Grafana login. Grafana's own port, if left
open, now answers under `/grafana/`. A controller restart invalidates the
cookies; the panel asks for a new one when it loads.

## ADR-032: Token analyzer

**Problem.** On phones every prompt token costs seconds (Qwen3-4B reads
about 12 tokens/s, see BENCHMARKS.md), and the context is 16k tokens. An
operator needs to know how long a prompt is for each served model before
sending it, and where the cluster's tokens go. Models tokenize differently,
so a character count or one generic tokenizer is not enough.

**Decision.**
- `POST /admin/tokenize {text, model?}` sends the text (up to 256 KiB) to
  llama-server's `/tokenize` on one phone per served model, in parallel, and
  returns each model's token count, its context size and the phone's
  self-test prompt speed, plus the token pieces for one model (up to 20,000).
  Tokenizing needs no slot, so busy phones answer too; external nodes are
  skipped because other servers have no `/tokenize`.
- The panel's **Tokens** view shows the counts with the share of the
  context, an estimated reading time on that phone and the text split into
  coloured tokens; below it, the token usage dashboard.
- Two more generated Grafana dashboards: `phoneborg-tokens` (totals, cache
  share, output per input, tokens by phone, model and key, request sizes)
  and `phoneborg-node` (one phone at a time: now, tokens, requests, health,
  model and Super Borg work). The Dashboards view switches between the
  cluster and the node dashboard and picks the phone by alias.

**Consequences.** Counts are exact for each model because the phones'
tokenizers are used; they include no chat template, so a chat request is a
few tokens longer. The reading time uses the self-test prompt speed from a
short prompt, so long prompts take longer than shown.

## ADR-033: Semantic router for "auto" (experimental)

**Problem.** `auto` sends every request to any ready phone. A greeting and
a request to write a red-black tree land on the same kind of phone, though
the small, fast models answer the first well and only the bigger ones the
second. Generation on a phone is slow (3–17 tok/s), prompt processing much
faster, so a decision that needs no generation is cheap enough to make per
request. The "semantic if" pattern (SemIf/OpenJev) reads such a decision
from the logits of a single forward pass; its reference code is Python,
which does not belong on the phones or in the controller.

**Alternatives.**
- Rules on the request (length, keywords, tools): cheap, but blind to what
  the request asks.
- A classifier answering in text or JSON: needs generation and output
  repair, and gives no confidence.
- SemIf's Python harness next to the controller, loading its own GGUF:
  a second runtime and a second copy of a model the phones already serve.
- Reading the answer probabilities over HTTP from a phone's own
  llama-server (chosen).

**Decision.**
- An optional router in the gateway, off by default, configured through
  `PUT /admin/gateway {"router": {...}}`, `pbctl gateway set router=on
  router_classifier=...`, `pbctl router ...` and the panel: a classifier
  target, a timeout (default 20 s) and a list of **classes**, each with a
  name, a description, optional example requests and a target.
- The classes are the operator's: 2–12 of them, added, changed and removed
  at will (the whole list is replaced on update; `reset_classes` restores
  the defaults). The defaults are six: easy/hard chat, writing, coding and
  reasoning (`easy_chat`, `easy_writing`, `hard_writing`, `easy_coding`,
  `hard_coding`, `hard_reasoning`). Class i answers with letter i (A, B,
  ...), a single token in every tokenizer we use.
- Only requests for `auto` are routed. The gateway sends the least busy
  classifier phone one chat completion with `max_tokens: 1`, `logprobs`,
  `top_logprobs: 20` and `enable_thinking: false`: a system prompt built
  from the classes (one line per class, then the examples; constant per
  configuration, so llama-server caches it) and the last user message (or
  the completion prompt) clipped to 2000 bytes. The class with the most
  probable letter wins; its target serves the request (empty = `auto`).
- Every failure is a fallback to plain `auto`, never a failed request,
  including answers that put less than 0.5 on the class letters together,
  which a small model gives when it starts doing the request instead of
  classifying it.
- The router, classes included, is stored with the pools and aliases in
  `routing.json` and loaded at start; an invalid stored router is logged
  and replaced by the defaults (off).
- Observability: `X-Phoneborg-Route: <class>|fallback` on the response,
  `phoneborg_gateway_router_decisions_total{result}`,
  `phoneborg_gateway_router_duration_seconds`, a log line per decision.

**Consequences.** An `auto` request waits for one classification first: on
a phone a few hundred prompt tokens, one to three seconds with the system
prompt cached. The classifier phone takes that load, and its in-flight
count shows it to the pickers. More classes and examples make the prompt,
and the first decision after a change, longer (17 s on the Pixel for the
six defaults). Overlapping classes split close requests between them; the
quality of the classes is the operator's to tune, checked with
`TestRouterLive`. The router uses llama-server's OpenAI-compatible logprobs
(verified against b11136); external engines that do not return
`top_logprobs` fall back every time. Models with sliding-window attention
(Gemma 3/3n) get no prompt-cache reuse for these requests in b11136, so a
decision costs the full prompt (30–40 s on the Mi 8); a Qwen3-4B phone
decides in about 2 s. Calibration per model and other decisions (retry,
verification in Super Borg) are left for later.
