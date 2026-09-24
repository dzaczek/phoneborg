# Benchmarks

Every measurement PhoneBorg has published, in one place. All numbers were
taken in 2026-09 on the development setup:

| Name used below | Hardware |
|---|---|
| **Mi 8** | Xiaomi Mi 8, Snapdragon 845 (4 big + 4 little Kryo 385 cores, FP16, no dotprod), 5.5 GiB RAM reported, LineageOS 22.2, USB 2.0 to the host. llama.cpp build `armv8.2-a+fp16`. |
| **phone-low** | redroid container on an Apple M2 host (colima), 2 CPUs, 2 GiB (`deploy/docker-compose.yml`) |
| **phone-mid** | the same, 4 CPUs, 3 GiB |

Emulated phones run on the host's M2 cores and memory. Their speed says
nothing about real phones; they are here to test the software path. See
[DEV_EMULATION.md](DEV_EMULATION.md#what-emulation-does-not-tell-you).

Unless stated otherwise, models are Q4_K_M GGUF files and tok/s figures come
from llama.cpp's own `timings` (prompt = prompt processing, generation =
token generation).

## Contents

- [Emulator vs Mi 8 baseline](#emulator-vs-mi-8-baseline)
- [Models on the Mi 8](#models-on-the-mi-8)
- [Threads on big.LITTLE](#threads-on-biglittle)
- [Screen-off endurance](#screen-off-endurance)
- [Failover under load](#failover-under-load)
- [Session affinity and the prompt cache](#session-affinity-and-the-prompt-cache)
- [OpenCode prompt sizes](#opencode-prompt-sizes)
- [Parallel agents: pool vs single node](#parallel-agents-pool-vs-single-node)
- [Effective bandwidth and performance tiers](#effective-bandwidth-and-performance-tiers)
- [Memory: what Android leaves free](#memory-what-android-leaves-free)
- [Other small measurements](#other-small-measurements)

## Emulator vs Mi 8 baseline

Qwen2.5-0.5B-Instruct Q4_K_M (468 MiB).

| | Mi 8 | phone-low (2 CPUs / 2 GiB) | phone-mid (4 CPUs / 3 GiB) |
|---|---|---|---|
| Threads | 6 | 2 | 4 |
| Prompt tok/s | ~23–37 | 108 | 204 |
| Generation tok/s | ~12–15 | 74 | 132 |

- **Mi 8:** the range covers the separate runs below (thread study: 23.8 /
  ~12 at 6 threads; model table: 36.2 / 14.8 at 8k context; endurance run:
  ~17 tok/s generation through the gateway).
- **Emulated:** `make llm-smoke` (llama-bench, then one chat completion
  through `adb forward`).

Reproduce:

```sh
make llama-all llm-smoke                        # every adb device
bash tests/e2e/llm_smoke.sh <SERIAL>            # one device, port 18091, does not disturb the agent
```

## Models on the Mi 8

**Setup.** Mi 8, `armv8.2-a+fp16` build, 6 threads, llama-server with an 8k
context, a ~300-token prompt and 64 generated tokens. With no model loaded,
3.1 GiB was available. RSS is llama-server's resident set size.

| Model (Q4_K_M) | File | RSS | Prompt tok/s | Gen tok/s | Verdict |
|---|---|---|---|---|---|
| Qwen2.5-0.5B-Instruct | 468 MiB | 632 MiB | 36.2 | 14.8 | fast, weak answers |
| Gemma 3 1B it | 768 MiB | 926 MiB | 16.7 | 7.3 | good |
| Llama 3.2 1B Instruct | 770 MiB | 1120 MiB | 15.8 | 8.4 | good |
| Qwen2.5-1.5B-Instruct | 1065 MiB | 1375 MiB | 11.4 | 6.8 | **best balance on this phone** |
| DeepSeek-R1-Distill-Qwen-1.5B | 1065 MiB | 1373 MiB | 9.3 | 6.0 | reasons step by step, so answers take long |
| Qwen3-1.7B | 1056 MiB | 2029 MiB | 8.8 | 4.5 | large KV cache per token |
| SmolLM3-3B | 1826 MiB | 2541 MiB | 4.3 | 2.9 | slow |
| Llama 3.2 3B Instruct | 1925 MiB | 2911 MiB | 4.2 | 2.6 | slow |
| Gemma 3 4B it | 2374 MiB | 2839 MiB | 3.8 | 2.4 | fits, too slow for chat |
| Qwen3-4B | 2381 MiB | 3248 MiB | 3.3 | 0.3 | memory pressure; unusable |
| Phi-4-mini (3.8B) | 2376 MiB | 3445 MiB | 3.4 | 0.6 | memory pressure |
| **Gemma 3n E2B it** | 2886 MiB | **1774 MiB** | 5.8 | 3.7 | **uses less RAM than its file; best 2–4B option** |
| Gemma 3n E4B it | 4328 MiB | 3032 MiB | 2.8 | 1.9 | fits a 5.5 GiB phone, slow |

**Gemma 3n E2B, later run.** After the resident-bytes change (ADR-012
update), the node was assigned the model by the controller and served it
through the gateway at a 16k context with a q8_0 KV cache, at 4.7 tok/s
generation. 1440 MiB of its 2886 MiB file is a per-layer embedding table
(`per_layer_token_embd.weight`) that llama.cpp reads only a few rows of per
token through mmap, which is why RSS stays at 1774 MiB.

**What the table shows.**

- Generation is bound by memory bandwidth: tok/s falls roughly in
  proportion to the resident model size (see
  [Effective bandwidth](#effective-bandwidth-and-performance-tiers)).
- Leave ~1 GiB of free memory unused. Qwen3-4B and Phi-4-mini fit on paper,
  but Android started evicting their pages and speed collapsed.
- KV cache size differs by architecture: Qwen3-1.7B needs ~4× more memory
  per context token than Qwen2.5-1.5B.
- `MemAvailable` barely moves when a model loads, because llama-server maps
  the weights from the file: no model 3156 MiB, Qwen2.5-0.5B 2984 MiB,
  Qwen2.5-1.5B 2970 MiB. Judge fit by RSS, not `MemAvailable`. The agent's
  sizing therefore counts only llama-server's `RssAnon` (ADR-012).

There is no script for this table; it was run by hand with the checks in
[REAL_PHONES.md](REAL_PHONES.md#6-verify) (`runtime.log` "prompt eval time"
lines, `top`).

## Threads on big.LITTLE

**Setup (ADR-009).** Mi 8, Qwen2.5-0.5B Q4, real agent + llama-server,
297-token prompt, 64 generated tokens, two interleaved series. Android
places the agent in the `foreground` cpuset (cores 0–3 little, 6–7 big), so
6 cores are usable.

| Threads | Prompt tok/s | Gen tok/s |
|---|---|---|
| 2 (= `-threads-policy big` here) | 13.4 | 8.8 |
| 4 | 19.0 | 9.3 |
| 6 (= `-threads-policy all`, the default) | 23.8 | ~12 |

**Traps.**

- Qualcomm `core_ctl` keeps only 2 of the 4 big cores online when idle.
  Benchmarks that pin threads with `taskset` stall on offline cores
  (0.1–0.5 tok/s) and do not reflect the unpinned server.
- More threads than usable cores is catastrophic.

Hence: benchmark through llama-server, not with pinned `llama-bench` runs.
Reproduce by provisioning with `-agent-args "-threads N"` and reading the
self-test in `pbctl nodes` (`TOK/S`) or `runtime.log`.

## Screen-off endurance

**Setup.** Mi 8 on USB power, screen off, "stay awake" disabled, 40 minutes.

**Result.** The node stayed `ACTIVE` the whole time. 40/40 requests were
served at ~17 tok/s, and no heartbeat was older than 5 s. On LineageOS
keeping the phone awake is therefore optional; vendor ROMs (MIUI, One UI)
may differ. Reproduce: turn the screen off and watch `pbctl nodes`.

## Failover under load

**Setup.** phone-low and phone-mid serving Qwen2.5-0.5B under load from
`tests/load/chat_load.py`, then phone-low is frozen with
`docker compose pause` (it goes `SUSPECT`, then `OFFLINE`).

**Result.**

- 0 failed requests out of 1138 while a phone was frozen. Load: 5 minutes of
  `tests/load/chat_load.py -d 300 -c 3 --stream 0.4` (3 concurrent clients,
  40% streaming); phone-low was paused for 30 s about 90 s into the run.
- In the e2e test (2 concurrent clients) the one request in flight on the
  frozen phone was cancelled when the node turned `SUSPECT` and retried on
  the other phone: 14 s instead of the 120 s upstream timeout.

Reproduce: `make e2e` (step "heartbeat loss under load").

## Session affinity and the prompt cache

**Setup.** opencode against phone-mid (4 cores), Qwen2.5-0.5B,
`-ctx-size 16384`, default build agent (~11.7k-token prompt).

| Turn | Time | Why |
|---|---|---|
| First turn of a new session | ~115 s | prompt processing of ~11.7k tokens |
| Later turns | ~3 s | session affinity keeps the session on one phone; 99.9% of the prompt comes from llama-server's cache |

On the Mi 8 the same cold prompt takes around 15 minutes (ADR-014). Grafana
shows the effect under "Prompt cache hit ratio" and "Routing decisions
(session affinity)".

## OpenCode prompt sizes

Measured with opencode 1.18.30.

| Agent | Prompt tokens |
|---|---|
| Default build agent | ~11.7k: ~4.2k system prompt, ~5.1k built-in tool definitions, ~2.1k MCP filesystem tools |
| Tool-less custom agent (`pbctl opencode` default) | ~182 |
| Same, with `-read-tools` (read, grep, glob) | about 1k more per call |

This gap is why `pbctl opencode` generates tool-less agents and why pools
default to `spread` routing (ADR-014).

## Parallel agents: pool vs single node

**Setup.** Mi 8 serving Qwen2.5-1.5B plus phone-low and phone-mid serving
Qwen2.5-0.5B. Three tool-less OpenCode agent tasks run in parallel.

| Target | Wall time | Correct answers |
|---|---|---|
| `pool/fast` (spread over 3 phones) | 9.9 s | 1 of 3 (the 0.5B phones got 2 wrong) |
| `node/mi8` (queued on one phone) | 23.8 s | 3 of 3 |

Spreading gives parallelism; pool membership decides quality. Use a pool
restricted to stronger models for tasks where correctness matters.

## Effective bandwidth and performance tiers

Generation streams the resident weights once per token, so
`gen_tok_s × resident_bytes` is roughly constant per phone. The controller
computes this "effective bandwidth" from each node's self-test and sorts
nodes into tiers (ADR-015): `t1` < 4, `t2` 4–10, `t3` 10–25, `t4` ≥ 25 GB/s.

| Phone | Model | Gen tok/s | Effective bandwidth | Tier |
|---|---|---|---|---|
| Mi 8 | Qwen2.5-0.5B (468 MiB) | 14.8 | 7.3 GB/s | t2 |
| Mi 8 | Qwen2.5-1.5B (1065 MiB) | 6.8 | 7.6 GB/s | t2 |
| phone-mid | Qwen2.5-0.5B | ~120 | ~56 GB/s | t4 |

Prompt processing scales the same way, more noisily (Mi 8: 17.8 and
12.7 GB/s for the two models).

ADR-015 first quoted 6.9 and 7.2 GB/s for the Mi 8, reading MiB as 10^6 bytes;
the values above use bytes, as the code does (`controller/models/perf_test.go`
checks 7.59 GB/s for the 1.5B case). Both are `t2`.

**Predicted vs measured on the Mi 8** (bandwidth 7.3–7.6 GB/s; predictions
are `bandwidth / resident_bytes`, computed from the numbers above):

| Model | Resident | Predicted | Measured |
|---|---|---|---|
| Gemma 3 4B it | 2374 MiB | 2.9–3.0 tok/s | 2.4 tok/s |
| Gemma 3n E2B it | 1446 MiB (file 2886 MiB) | 4.8–5.0 tok/s (2.4–2.5 if the file size were used) | 3.7 tok/s (benchmark), 4.7 tok/s (later run) |

The model ignores sliding-window attention and compute buffers, so it runs
a little optimistic. It is good enough to keep a phone from being given a
model that is categorically too slow (`-min-predicted-tok-s`, default 3).
See the `PRED TOK/S` column of `pbctl placement`.

## Memory: what Android leaves free

| Phone | Used by Android and apps | Source |
|---|---|---|
| phone-low (2 GiB) | ~0.65 GiB, leaving ~1.35 GiB for inference | emulator run; the agent's `MemoryBudget` (ADR-011/012) |
| phone-mid (3 GiB) | ~0.65 GiB | agent's `MemoryBudget` |
| Mi 8 (5.5 GiB) | ~2.4 GiB | agent's `MemoryBudget` |

The catalog's class heuristic assumes a flat 2 GiB; placement uses the
node's reported budget once it has one (ADR-011 update).

On phone-low a 0.5B Q4 model fits and 1.5B Q4 is tight.

## Other small measurements

| What | Result |
|---|---|
| Synthetic CPU benchmark (`synthetic-go-v0`) | Mi 8 7.4 GFLOPS vs phone-low 8.6, although the Mi 8 generates ~12 tok/s against 74. This is why routing uses the llama.cpp self-test (ADR-010). |
| First push of a 0.5 GB model over USB 2.0 | ~30 s; later runs skip it when the file size matches |
| e2e test duration | ~1–2 min (`make e2e`) |
