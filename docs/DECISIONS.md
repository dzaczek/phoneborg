# Architecture Decision Records

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
