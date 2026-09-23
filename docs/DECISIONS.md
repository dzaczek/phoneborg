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
