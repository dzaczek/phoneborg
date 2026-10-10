# Benchmarks

Every measurement PhoneBorg has published, in one place. Most numbers were
taken in 2026-09 on the development setup below; the
[multi-model benchmark](#multi-model-benchmark-four-phones) and the
[Super Borg timings](#super-borg-and-jobs-observed-timings) come from the
four-phone production cluster in 2026-10.

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
- [Multi-model benchmark, four phones](#multi-model-benchmark-four-phones)
- [Super Borg and jobs: observed timings](#super-borg-and-jobs-observed-timings)
- [Semantic router classifier](#semantic-router-classifier)
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

## Multi-model benchmark, four phones

Measured on 2026-10-04 with MMB (ADR-028, `pbctl mmb run nodes=pixel,mi8,oneplus,poco`,
one phone after the other) on the production cluster:

| Name | Hardware | Android | RAM / budget for the model |
|---|---|---|---|
| **oneplus** | OnePlus 10 Pro (NE2213), Snapdragon 8 Gen 1 (SM8450) | 16 | 11.0 GiB / ~7 GiB |
| **poco** | POCO F3 (M2012K11AG), Snapdragon 870 (SM8250) | 13 | 7.3 GiB / ~4.4 GiB |
| **pixel** | Google Pixel 8 Pro, Tensor G3 | 17 | 11.3 GiB / ~6–7 GiB |
| **mi8** | Xiaomi Mi 8, Snapdragon 845 | 15 (LineageOS) | 5.5 GiB / ~2.6 GiB |

**Method.** For every ready catalog model that fits a phone (RAM class and
budget), the phone is drained, the model is forced onto it, and three
requests are timed: a ~500-token prompt with an empty cache (*cold*), the
same prefix with another question (*warm*: the answer after the first
prompt), and a one-line question. Generation is a fixed 128 tokens
(`ignore_eos`), temperature 0, thinking off. Speeds are llama.cpp's own
timings; times are measured by the controller. `n/f` = does not fit the
phone. 65 results, 11 skipped, no failures. Context 16k for all; the POCO
used a q8_0 KV cache for the 4B models, the others f16.

**Generation, tok/s (128 tokens)**

| Model | Size | oneplus | poco | pixel | mi8 |
|---|---|---|---|---|---|
| qwen2.5-0.5b-instruct-q4_k_m | 0.5 GiB | 29.0 | 25.3 | 28.3 | 14.6 |
| gemma-3-1b-it-q4_k_m | 0.8 GiB | 17.0 | 14.2 | 15.2 | 7.0 |
| llama-3.2-1b-instruct-q4_k_m | 0.8 GiB | 17.2 | 16.6 | 15.9 | 7.2 |
| qwen3-1.7b-q4_k_m | 1.0 GiB | 11.6 | 9.4 | 11.6 | 4.9 |
| qwen2.5-1.5b-instruct-q4_k_m | 1.0 GiB | 13.3 | 12.8 | 13.5 | 5.7 |
| deepseek-r1-distill-qwen-1.5b-q4_k_m | 1.0 GiB | 13.5 | 12.7 | 13.5 | 5.7 |
| lfm2-2.6b-q4_k_m | 1.5 GiB | 10.0 | 9.8 | 8.8 | 3.8 |
| gemma-2-2b-it-abliterated-q4_k_m | 1.6 GiB | 6.6 | 6.2 | 6.2 | 3.0 |
| huggingfacetb_smollm3-3b-q4_k_m | 1.8 GiB | 7.1 | 7.0 | 6.4 | n/f |
| granite-4.0-h-micro-q4_k_m | 1.8 GiB | 6.7 | 6.7 | 3.1 | n/f |
| llama-3.2-3b-instruct-q4_k_m | 1.9 GiB | 6.4 | 6.0 | 6.0 | n/f |
| dolphin3.0-llama3.2-3b-q4_k_m | 1.9 GiB | 6.4 | 6.3 | 5.9 | n/f |
| gemma-3-4b-it-abliterated.q4_k_m | 2.3 GiB | 5.0 | 5.2 | 3.5 | n/f |
| gemma-3-4b-it-q4_k_m | 2.3 GiB | 5.1 | 5.4 | 4.6 | n/f |
| phi-4-mini-instruct-q4_k_m | 2.3 GiB | 5.1 | 5.7 | 2.6 | n/f |
| qwen3-4b-abliterated.q4_k_m | 2.3 GiB | 4.8 | 5.0 | 2.8 | n/f |
| qwen3-4b-instruct-2507-q4_k_m | 2.3 GiB | 4.6 | 5.0 | 4.5 | n/f |
| gemma-3n-e2b-it-q4_k_m | 2.8 GiB | 6.1 | 7.0 | 5.8 | 3.2 |
| qwen3-8b-q4_k_m | 4.7 GiB | 2.9 | n/f | 1.8 | n/f |

**Prompt processing, tok/s (~500-token prompt)**

| Model | Size | oneplus | poco | pixel | mi8 |
|---|---|---|---|---|---|
| qwen2.5-0.5b-instruct-q4_k_m | 0.5 GiB | 89.2 | 88.0 | 123.5 | 36.6 |
| gemma-3-1b-it-q4_k_m | 0.8 GiB | 44.5 | 45.2 | 62.5 | 15.1 |
| llama-3.2-1b-instruct-q4_k_m | 0.8 GiB | 59.2 | 59.0 | 98.4 | 13.0 |
| qwen3-1.7b-q4_k_m | 1.0 GiB | 47.9 | 39.3 | 59.5 | 8.4 |
| qwen2.5-1.5b-instruct-q4_k_m | 1.0 GiB | 51.9 | 42.3 | 67.8 | 8.9 |
| deepseek-r1-distill-qwen-1.5b-q4_k_m | 1.0 GiB | 47.3 | 42.0 | 67.5 | 8.9 |
| lfm2-2.6b-q4_k_m | 1.5 GiB | 23.6 | 23.6 | 28.3 | 4.9 |
| gemma-2-2b-it-abliterated-q4_k_m | 1.6 GiB | 24.2 | 26.4 | 30.7 | 5.8 |
| huggingfacetb_smollm3-3b-q4_k_m | 1.8 GiB | 17.0 | 19.7 | 19.7 | n/f |
| granite-4.0-h-micro-q4_k_m | 1.8 GiB | 13.8 | 16.5 | 21.9 | n/f |
| llama-3.2-3b-instruct-q4_k_m | 1.9 GiB | 16.3 | 19.5 | 24.0 | n/f |
| dolphin3.0-llama3.2-3b-q4_k_m | 1.9 GiB | 15.9 | 19.8 | 19.1 | n/f |
| gemma-3-4b-it-abliterated.q4_k_m | 2.3 GiB | 13.4 | 16.2 | 17.3 | n/f |
| gemma-3-4b-it-q4_k_m | 2.3 GiB | 13.7 | 16.1 | 14.8 | n/f |
| phi-4-mini-instruct-q4_k_m | 2.3 GiB | 14.3 | 15.9 | 18.2 | n/f |
| qwen3-4b-abliterated.q4_k_m | 2.3 GiB | 12.1 | 13.9 | 16.2 | n/f |
| qwen3-4b-instruct-2507-q4_k_m | 2.3 GiB | 11.7 | 12.6 | 12.9 | n/f |
| gemma-3n-e2b-it-q4_k_m | 2.8 GiB | 19.2 | 24.2 | 21.5 | 5.9 |
| qwen3-8b-q4_k_m | 4.7 GiB | 6.1 | n/f | 6.9 | n/f |

**Time to first token: cold → warm (same ~500-token prefix again), s**

| Model | Size | oneplus | poco | pixel | mi8 |
|---|---|---|---|---|---|
| qwen2.5-0.5b-instruct-q4_k_m | 0.5 GiB | 4.3 → 0.3 | 4.4 → 0.3 | 3.2 → 0.5 | 13.2 → 0.5 |
| gemma-3-1b-it-q4_k_m | 0.8 GiB | 10.2 → 8.9 | 8.3 → 8.6 | 6.1 → 9.1 | 29.6 → 25.3 |
| llama-3.2-1b-instruct-q4_k_m | 0.8 GiB | 6.5 → 0.4 | 7.7 → 0.4 | 4.0 → 1.7 | 33.2 → 1.3 |
| qwen3-1.7b-q4_k_m | 1.0 GiB | 8.1 → 0.6 | 10.0 → 0.8 | 8.8 → 0.8 | 55.7 → 2.6 |
| qwen2.5-1.5b-instruct-q4_k_m | 1.0 GiB | 7.4 → 0.5 | 9.1 → 0.5 | 5.8 → 4.4 | 56.0 → 1.8 |
| deepseek-r1-distill-qwen-1.5b-q4_k_m | 1.0 GiB | 8.1 → 0.4 | 9.1 → 0.4 | 5.7 → 3.3 | 52.3 → 1.6 |
| lfm2-2.6b-q4_k_m | 1.5 GiB | 17.7 → 19.9 | 18.9 → 16.8 | 17.6 → 17.4 | 104.5 → 80.8 |
| gemma-2-2b-it-abliterated-q4_k_m | 1.6 GiB | 16.4 → 16.3 | 18.6 → 14.3 | 16.5 → 16.2 | 86.6 → 64.2 |
| huggingfacetb_smollm3-3b-q4_k_m | 1.8 GiB | 43.9 → 1.3 | 37.2 → 1.2 | 31.3 → 1.5 | n/f |
| granite-4.0-h-micro-q4_k_m | 1.8 GiB | 35.8 → 28.9 | 29.8 → 23.5 | 23.3 → 27.2 | n/f |
| llama-3.2-3b-instruct-q4_k_m | 1.9 GiB | 29.5 → 1.1 | 25.2 → 1.1 | 22.9 → 1.3 | n/f |
| dolphin3.0-llama3.2-3b-q4_k_m | 1.9 GiB | 28.2 → 1.2 | 26.3 → 1.0 | 54.7 → 1.3 | n/f |
| gemma-3-4b-it-abliterated.q4_k_m | 2.3 GiB | 36.3 → 28.2 | 30.1 → 24.5 | 83.3 → 27.2 | n/f |
| gemma-3-4b-it-q4_k_m | 2.3 GiB | 38.5 → 28.1 | 33.6 → 23.0 | 58.2 → 28.5 | n/f |
| phi-4-mini-instruct-q4_k_m | 2.3 GiB | 32.7 → 1.1 | 31.7 → 1.0 | 28.8 → 1.1 | n/f |
| qwen3-4b-abliterated.q4_k_m | 2.3 GiB | 43.2 → 1.8 | 37.3 → 2.3 | 27.0 → 1.9 | n/f |
| qwen3-4b-instruct-2507-q4_k_m | 2.3 GiB | 43.2 → 1.5 | 38.0 → 1.8 | 40.1 → 9.0 | n/f |
| gemma-3n-e2b-it-q4_k_m | 2.8 GiB | 23.7 → 19.9 | 19.7 → 15.9 | 32.0 → 19.8 | 83.8 → 66.6 |
| qwen3-8b-q4_k_m | 4.7 GiB | 82.9 → 3.4 | n/f | 100.0 → 3.3 | n/f |

**Load time (first load includes the download to the phone), s**

| Model | Size | oneplus | poco | pixel | mi8 |
|---|---|---|---|---|---|
| qwen2.5-0.5b-instruct-q4_k_m | 0.5 GiB | 8 | 8 | 6 | 6 |
| gemma-3-1b-it-q4_k_m | 0.8 GiB | 6 | 34 | 32 | 12 |
| llama-3.2-1b-instruct-q4_k_m | 0.8 GiB | 36 | 34 | 32 | 36 |
| qwen3-1.7b-q4_k_m | 1.0 GiB | 42 | 46 | 42 | 42 |
| qwen2.5-1.5b-instruct-q4_k_m | 1.0 GiB | 44 | 42 | 42 | 12 |
| deepseek-r1-distill-qwen-1.5b-q4_k_m | 1.0 GiB | 12 | 44 | 42 | 44 |
| lfm2-2.6b-q4_k_m | 1.5 GiB | 60 | 60 | 60 | 52 |
| gemma-2-2b-it-abliterated-q4_k_m | 1.6 GiB | 66 | 62 | 64 | 58 |
| huggingfacetb_smollm3-3b-q4_k_m | 1.8 GiB | 66 | 74 | 68 | n/f |
| granite-4.0-h-micro-q4_k_m | 1.8 GiB | 66 | 74 | 68 | n/f |
| llama-3.2-3b-instruct-q4_k_m | 1.9 GiB | 74 | 80 | 66 | n/f |
| dolphin3.0-llama3.2-3b-q4_k_m | 1.9 GiB | 18 | 76 | 72 | n/f |
| gemma-3-4b-it-abliterated.q4_k_m | 2.3 GiB | 20 | 26 | 88 | n/f |
| gemma-3-4b-it-q4_k_m | 2.3 GiB | 82 | 88 | 84 | n/f |
| phi-4-mini-instruct-q4_k_m | 2.3 GiB | 86 | 88 | 82 | n/f |
| qwen3-4b-abliterated.q4_k_m | 2.3 GiB | 84 | 24 | 90 | n/f |
| qwen3-4b-instruct-2507-q4_k_m | 2.3 GiB | 20 | 26 | 28 | n/f |
| gemma-3n-e2b-it-q4_k_m | 2.8 GiB | 20 | 102 | 114 | 30 |
| qwen3-8b-q4_k_m | 4.7 GiB | 38 | n/f | 182 | n/f |

**What the numbers say**

- **Phones.** On the same model, the OnePlus, POCO and Pixel are close
  (within ~10 %); the Mi 8 is about half as fast. Only the OnePlus and the
  Pixel can hold the 8B model. The Pixel has the fastest prompt processing
  on small models (123 tok/s on the 0.5B).
- **The Pixel's numbers for 3B+ models are low and inconsistent**: two 4B
  models of the same size gave 4.5 and 2.8 tok/s, and the Qwen3-4B-Instruct
  measured 4.9 tok/s in a short benchmark the day before. It ran first and
  loaded models for over an hour; Tensor G3 is known to throttle. Treat its
  large-model figures as a lower bound and re-measure after it cools down.
- **Prompt cache by architecture.** The warm request should only process the
  new question. That works for Qwen, Llama, Phi, SmolLM and DeepSeek-R1
  (warm TTFT 0.3–1.8 s on the OnePlus). It does **not** work for Gemma 2/3,
  Gemma 3n, LFM2 and Granite 4.0-H: their warm TTFT is close to the cold one
  (Gemma 3n on the Mi 8: 84 s cold, 67 s warm). These use sliding-window
  attention or hybrid recurrent layers, for which llama.cpp re-processes the
  prompt; `--swa-full` may restore reuse for Gemma at a memory cost (not
  tested yet). In conversations and agent sessions these models are much
  slower than their tok/s suggest.
- **Size vs speed.** Generation speed falls roughly with the resident size
  (bandwidth-bound, see [effective bandwidth](#effective-bandwidth-and-performance-tiers)):
  ~25–29 tok/s for 0.5B, ~13–17 for 1–1.5B, ~6–7 for 3B, ~5 for 4B, ~3 for
  8B on the faster phones.
- **The 8B model is slow to read.** Qwen3-8B on the OnePlus processes
  prompts at 6.1 tok/s: 500 tokens take 83 s, an opencode build-agent prompt
  (~11.7k tokens) would take about half an hour. This is what makes a phone
  a slow Super Borg orchestrator.
- **DeepSeek-R1-Distill 1.5B returned empty answers** to the one-line
  question: it reasons even with thinking off and spends the token budget
  before answering. It is unsuitable for short answers and as a worker.
- **Load time** is dominated by the first download (2–3 min for 2–5 GB over
  USB 2.0); a model already on the phone loads in 6–40 s.
- Best picks from this run: **Llama 3.2 1B** for fast small tasks (16–17
  tok/s, warm 0.4 s), **Qwen3-1.7B** for a small model with tools on the
  Mi 8, **Qwen3-4B-Instruct** and **Phi-4-mini** for 4B work with a working
  cache, **Qwen3-8B** only as an orchestrator on the OnePlus.

## Super Borg and jobs: observed timings

Not a controlled benchmark: timings seen while building and using Super
Borg (ADR-020..022) on the four phones, useful to set expectations.

| What | Setup | Time |
|---|---|---|
| Chat, three-part request ("poem, translation, three facts") | orchestrator Qwen3-4B on the POCO, two workers | ~2 min |
| Same request | orchestrator Qwen3-8B on the OnePlus, three workers | ~6 min (most of it the orchestrator writing the answer) |
| Same request with thinking on | Qwen3-8B on the OnePlus | over 10 min; cut at the old 600 s total limit, which led to the first-token/idle limits of ADR-024 |
| One job step (orchestrator call) | Qwen3-8B on the OnePlus, prompt ~2–3k tokens | 5–10 min |
| A chapter written one at a time | worker gemma-3n on the Mi 8 | ~15 min per chapter (step + writing) |
| A 12-document story (plan, outline, chapters) | gateway assigns workers, three phones in parallel | ~70 min, 4 documents per phone |

Most of the time goes to the orchestrator reading its prompt on a phone.
An external orchestrator (ADR-029) removes that part; the writing time on
the phones stays.

## Semantic router classifier

One-token classification of `auto` requests (ADR-033), llama.cpp b11136,
measured with `TestRouterLive` (12 requests, 2 per default class, none of
them among the prompt's examples) and with the earlier two-class
easy/hard prompt (7 requests).

| Classifier | Prompt | Right | Per decision, prompt cached | First decision |
|---|---|---|---|---|
| Pixel 8 Pro, Qwen3-4B-Instruct-2507 | 6 default classes | 11 of 12 | 1.7–2.7 s | 17 s |
| Pixel 8 Pro, Qwen3-4B-Instruct-2507 | easy/hard | 7 of 7 | 1.4–1.8 s | 9 s |
| Mi 8, Gemma 3n E2B | easy/hard | 7 of 7 | 30–41 s | 30 s |
| Mac (Metal), Gemma 3 1B / Gemma 3n E2B / Llama 3.2 3B | easy/hard | 7 of 7 each | 0.05–0.25 s | |
| Mac (Metal), Llama 3.2 1B | easy/hard | 3–4 of 7 | 0.02 s | |

- The miss with six classes: "Plan a migration of our monolith to
  microservices, with rollback steps" (hard_reasoning) went to
  hard_writing with p = 0.70.
- Gemma 3/3n never reuse the cached prompt here: they use sliding-window
  attention, and llama-server can only roll back to a context checkpoint
  near the end of the previous prompt, which a different request
  invalidates (log: `erased invalidated context checkpoint`), so the Mi 8
  reprocesses ~250 tokens at ~6 tok/s every time. The same prefix with a
  different 5–9-token tail is reused (about 1 s), which is why it is not
  obvious in a quick test.
- Llama 3.2 1B often starts doing the request (first token a code fence)
  instead of answering a letter; the gateway treats answers with less
  than 0.5 on the class letters as a fallback.
- End to end on the cluster (classifier `node/pixel`, 2026-10-10): six
  requests, one per class, all classified right (p = 1.0) and served by
  their targets (Mi 8, `pool/translate`, `pool/writer`, `pool/fast`,
  `pool/code`, `pool/think`); 17.5 s for the first, 1.8–2.2 s for the
  rest.

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
