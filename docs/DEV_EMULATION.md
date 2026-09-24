# Developer environment: emulated phones

This guide is for developing PhoneBorg without real phones. Design context:
[ARCHITECTURE.md](ARCHITECTURE.md); ADR-004 explains why redroid.

`deploy/docker-compose.yml` starts two emulated Android 12 arm64 phones plus the
controller, Prometheus and Grafana. The phones are reached through **real adb**
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

## What the e2e test checks

`tests/e2e/redroid_e2e.sh` (`make e2e`):

- adb connect → ABI check → push → `adb reverse` → detached start works on real Android
- agent registers, inventory reports the **container's** RAM/CPU limits
- benchmark → `ACTIVE`; the model is served on both phones through the
  gateway, and a `spread` pool reaches both
- freezing a phone under load → `SUSPECT` → `OFFLINE` with zero failed
  requests → recovers to `ACTIVE`
- controller restart → agents re-register on their own

`pcprov watch` re-provisioning a replugged device is not covered by the e2e
test; check it by hand.

On the 2 GiB profile Android itself uses ~0.65 GiB, leaving ~1.35 GiB for
inference ([BENCHMARKS.md](BENCHMARKS.md#memory-what-android-leaves-free)).

## What emulation does not tell you

| Area | redroid | Real phone — check on first device |
|------|---------|-------------------------------------|
| CPU speed | Apple M2 cores, only the core **count** is limited | Big.LITTLE, much slower; benchmark numbers are not comparable |
| Memory bandwidth | ~56 GB/s effective (tier `t4`) | ~7 GB/s effective on a Mi 8 (tier `t2`) |
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

Emulated phones run roughly 5–10× faster than a real Mi 8
([BENCHMARKS.md](BENCHMARKS.md#emulator-vs-mi-8-baseline)). The model fits on
the 2 GiB profile. The RAM check counts page cache in cgroup
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

Any OpenAI client works with `base_url=http://127.0.0.1:18080/v1`; the
`X-PhoneBorg-Node` response header shows which phone answered.
`tests/load/chat_load.py` flags: `--url`, `--key`, `--model` (also
`pool/<name>` etc.), `-n` requests or `-d` seconds, `-c` concurrency,
`--stream` fraction, `--max-tokens`.

For API keys, pools, opencode and the rest of day-to-day use, see
[OPERATIONS.md](OPERATIONS.md). Failover and prompt-cache measurements from
this setup are in [BENCHMARKS.md](BENCHMARKS.md#failover-under-load).
