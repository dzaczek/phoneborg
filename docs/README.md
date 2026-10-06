# PhoneBorg documentation

Start with the [project README](../README.md) for the pitch and quick start.

## Operators: set up, run, manage

| Document | Covers |
|---|---|
| [QUICKSTART.md](QUICKSTART.md) | fast track: a test cluster with one or two phones in 30–45 minutes, from build to pools, Super Borg and a job |
| [CONCEPTS.md](CONCEPTS.md) | ways to use the cluster (models, pools, Super Borg with an internal or external orchestrator, jobs, agents through MCP, benchmarks) with their pros and cons, a comparison and which one to pick |
| [REAL_PHONES.md](REAL_PHONES.md) | choosing phones, preparing them, adb checks, provisioning, verification, slim, model storage, files and logs on the phone, removal, phone-side troubleshooting, which model suits which phone |
| [OPERATIONS.md](OPERATIONS.md) | controller flags, sending requests, web panel, full `pbctl` reference, models and placement, pools and aliases, API keys, draining, gateway settings, usage, OpenCode bridge, Grafana, controller-side troubleshooting |
| [BENCHMARKS.md](BENCHMARKS.md) | what to expect: tok/s per model, threads, endurance, failover, prompt cache, parallel agents, bandwidth tiers, the four-phone multi-model benchmark, Super Borg and job timings |

## Developers: architecture, decisions, tests

| Document | Covers |
|---|---|
| [ARCHITECTURE.md](ARCHITECTURE.md) | components, provisioning, node lifecycle, request routing, model placement and switching, persistence, ports, security model, limitations |
| [DECISIONS.md](DECISIONS.md) | ADR-001..031 with an index and dated updates |
| [DEV_EMULATION.md](DEV_EMULATION.md) | colima + redroid setup, `make test`, `make e2e`, `make llm-smoke`, load generator |

## I want to…

| Task | Go to |
|---|---|
| set up a test cluster with one or two phones, fast | [QUICKSTART.md](QUICKSTART.md) |
| choose between pools, Super Borg, jobs and agents | [CONCEPTS.md](CONCEPTS.md#which-one-should-i-use) |
| compare models and phones on real numbers | [CONCEPTS.md](CONCEPTS.md#7-benchmarks-to-choose-models-and-phones), [BENCHMARKS.md](BENCHMARKS.md#multi-model-benchmark-four-phones) |
| plug in a desktop or API model as orchestrator | [OPERATIONS.md](OPERATIONS.md#external-orchestrators) |
| add a real phone | [REAL_PHONES.md](REAL_PHONES.md#1-prepare-the-phone-once-per-phone) |
| try it without phones | [DEV_EMULATION.md](DEV_EMULATION.md#run) |
| pick a model for my phones | [REAL_PHONES.md](REAL_PHONES.md#choosing-a-model-per-phone-class) |
| send a request / use the OpenAI SDK | [OPERATIONS.md](OPERATIONS.md#sending-requests) |
| use the cluster from opencode | [OPERATIONS.md](OPERATIONS.md#opencode-agent-bridge) |
| add a model and assign it to phones | [OPERATIONS.md](OPERATIONS.md#models-and-placement) |
| give agents their own phones (pools, aliases) | [OPERATIONS.md](OPERATIONS.md#virtual-models-pools-and-aliases) |
| require API keys | [OPERATIONS.md](OPERATIONS.md#api-keys) |
| take a phone out for maintenance | [OPERATIONS.md](OPERATIONS.md#draining-a-phone-for-maintenance) |
| look up a `pbctl` command | [OPERATIONS.md](OPERATIONS.md#pbctl-reference) |
| fix a phone that drops out or crashes | [REAL_PHONES.md](REAL_PHONES.md#troubleshooting) |
| understand an error from the gateway | [OPERATIONS.md](OPERATIONS.md#troubleshooting) |
| free RAM on a dedicated phone | [REAL_PHONES.md](REAL_PHONES.md#free-ram-on-a-dedicated-phone-slim) |
| know which ports are used | [ARCHITECTURE.md](ARCHITECTURE.md#ports) |
| know what is unauthenticated | [ARCHITECTURE.md](ARCHITECTURE.md#security-model) |
| reproduce a measurement | [BENCHMARKS.md](BENCHMARKS.md) |
| learn why something was designed this way | [DECISIONS.md](DECISIONS.md) |
