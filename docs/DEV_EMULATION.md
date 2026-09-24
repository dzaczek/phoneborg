# Testing without phones: redroid emulation

`deploy/docker-compose.yml` starts two emulated Android 12 arm64 phones plus the
controller and Prometheus. The phones are reached through **real adb**
(`127.0.0.1:5555`, `127.0.0.1:5556`), so the same `pcprov` / node-agent path is
used as with USB phones.

| Service     | RAM (default) | CPUs | Env override                        |
|-------------|---------------|------|-------------------------------------|
| `phone-low` | 2 GiB         | 2    | `PHONE_LOW_MEM`, `PHONE_LOW_CPUS`   |
| `phone-mid` | 3 GiB         | 4    | `PHONE_MID_MEM`, `PHONE_MID_CPUS`   |

## One-time host setup (macOS, Apple Silicon)

```sh
brew install colima docker docker-compose
colima start --cpu 6 --memory 8 --disk 40   # bigger phones need more --memory
sh deploy/colima-binder.sh                   # binder driver for redroid; re-run after VM restart
```

If pulls fail with `docker-credential-osxkeychain: executable file not found`,
your `~/.docker/config.json` still references Docker Desktop: remove the
`credsStore` line or `brew install docker-credential-helper`.

## Run

```sh
make test                 # unit tests
make e2e                  # full end-to-end run (≈1–2 min)
```

Manual:

```sh
make agent pcprov cluster-up
bin/pcprov provision -connect 127.0.0.1:5555 -connect 127.0.0.1:5556
open http://127.0.0.1:18080          # web panel (token: deploy/dev-admin-token); plain table at /status
open http://127.0.0.1:9090           # Prometheus, try phoneborg_nodes
open http://127.0.0.1:3000           # Grafana, "PhoneBorg" dashboard (admin/admin to edit)
docker compose -f deploy/docker-compose.yml pause phone-low   # simulate hang -> SUSPECT -> OFFLINE
docker compose -f deploy/docker-compose.yml unpause phone-low # -> ACTIVE
make cluster-down
```

## What the e2e test proves

- adb connect → ABI check → push → `adb reverse` → detached start works on real Android
- agent registers, inventory reports the **container's** RAM/CPU limits
- benchmark → `ACTIVE`; frozen phone → `SUSPECT` → `OFFLINE` → recovers
- controller restart → agents re-register on their own
- `pcprov watch` re-provisions a device that disappears and comes back

Measured on the 2 GiB profile: Android itself uses ~0.65 GiB, leaving ~1.35 GiB
for inference. A 0.5B Q4 GGUF fits; 1.5B Q4 is tight.

## What emulation does NOT tell you about real phones

| Area | redroid | Real phone — check on first device |
|------|---------|-------------------------------------|
| CPU speed | Apple M2 cores, only the core **count** is limited | Big.LITTLE, much slower; benchmark numbers are not comparable |
| Memory bandwidth | ~65 GB/s (host) | ~10–30 GB/s |
| Thermals / throttling | no thermal zones, temperature is `n/a` | zones may be SELinux-blocked for `shell`; falls back to battery temp |
| Battery | none | reported via `dumpsys battery` |
| USB authorisation | not needed | tap "Allow USB debugging"; `pcprov watch` logs `unauthorized` until then |
| Process lifetime | never killed | some ROMs (Xiaomi, Samsung) may kill shell processes when the screen is off; agent does not survive reboot (ADR-001) |
| `/proc`, `getprop` | stock AOSP | vendor props differ; inventory must not assume any |

Recommended first real-phone check: `bin/pcprov watch`, plug the phone in,
accept the prompt, confirm it becomes `ACTIVE`, then leave it with the screen
off for 30 minutes and confirm it stays `ACTIVE`.

## Running a small LLM on the phones

```sh
make llama-all    # static arm64 llama-bench + llama-server, all CPU variants (ADR-005/ADR-007)
make llm-smoke    # downloads Qwen2.5-0.5B-Instruct Q4_K_M (~470 MiB), runs on every adb device
bash tests/e2e/llm_smoke.sh 127.0.0.1:5555   # one device
```

For each device the smoke test picks the fastest llama.cpp build the phone's
CPU supports (same selection pcprov does, ADR-007) and checks free RAM (model
+ 50%), pushes the binaries and the model to `/data/local/tmp/phoneborg`, runs
`llama-bench`, then starts `llama-server` and sends one
`/v1/chat/completions` request through `adb forward`.

Measured on redroid (host M2 cores, so real phones will be several times
slower):

| Profile | Threads | Prompt tok/s | Generation tok/s |
|---------|---------|--------------|------------------|
| phone-low (2 GiB) | 2 | 108 | 74 |
| phone-mid (3 GiB) | 4 | 204 | 132 |

The model fits on the 2 GiB profile. The RAM check counts page cache in cgroup
`memory.current`, so it is conservative.

After a colima restart: `sh deploy/colima-binder.sh` (makes binder load at VM
boot). If phones were started without binder, run
`docker compose -f deploy/docker-compose.yml restart phone-low phone-mid`.

## Serving through the gateway (one API for the whole cluster)

```sh
make llama-all agent pcprov cluster-up
bin/pcprov provision -connect 127.0.0.1:5555 -connect 127.0.0.1:5556 \
  -model models/qwen2.5-0.5b-instruct-q4_k_m.gguf
curl -s http://127.0.0.1:18080/v1/models
curl -s http://127.0.0.1:18080/v1/chat/completions -H 'Content-Type: application/json' \
  -d '{"messages":[{"role":"user","content":"Hello!"}],"max_tokens":32}'
python3 tests/load/chat_load.py -d 120 -c 3     # traffic for the Grafana dashboard
```

Any OpenAI client works with `base_url=http://127.0.0.1:18080/v1`. The
`X-PhoneBorg-Node` response header shows which phone answered.

API keys: create a file with one `<name> <key>` pair per line and start the
controller with `-api-keys-file`. Clients send `Authorization: Bearer <key>`
(or `x-api-key`). Metrics are labelled with the key's name. To create and
revoke hashed keys at runtime with `pbctl`, see [USAGE.md](USAGE.md#api-keys).

Measured with the e2e test: freezing a phone under load (2 concurrent clients)
caused zero failed requests. The one request that was in flight on the frozen
phone was cancelled when the node became SUSPECT and retried on the other
phone (14 s instead of the 120 s timeout).

## Using the cluster from opencode

opencode's system prompt and tool definitions are ~11.7k tokens, so phones
need a larger context than the default 2048:

```sh
bin/pcprov provision -connect 127.0.0.1:5555 -connect 127.0.0.1:5556 \
  -model models/qwen2.5-0.5b-instruct-q4_k_m.gguf -agent-args "-ctx-size 16384"
```

Provider entry (`~/.config/opencode/opencode.jsonc`, under `"provider"`):

```jsonc
"phoneborg": {
  "npm": "@ai-sdk/openai-compatible",
  "name": "PhoneBorg (phones)",
  "options": { "baseURL": "http://127.0.0.1:18080/v1", "timeout": false,
               "headerTimeout": 1800000, "chunkTimeout": 1800000 },
  "models": { "qwen2.5-0.5b-instruct-q4_k_m": {
    "name": "Qwen2.5 0.5B on phones", "limit": { "context": 16384, "output": 2048 } } }
}
```

Run `opencode -m phoneborg/qwen2.5-0.5b-instruct-q4_k_m`. Measured on
redroid: the first turn of a new session takes ~115 s (prompt processing on the
4-core phone). Later turns take ~3 s, because session affinity keeps the
session on the same phone and >99% of the prompt comes from llama-server's
cache. Grafana shows this under "Prompt cache hit ratio" and "Routing
decisions".
A 0.5B model is too small for reliable tool use; this setup proves the
integration, not coding quality.
