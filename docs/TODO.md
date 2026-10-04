# TODO

Planned work, not implemented yet. Each item starts with a manual proof of
concept on the real cluster; build it into PhoneBorg only if the PoC meets
its go criteria.

- [ ] [Image generation engine on phones](#1-image-generation-engine-on-phones)
- [ ] [Laya: smart routing for the phone cluster](#2-laya-smart-routing-for-the-phone-cluster)

## 1. Image generation engine on phones

Status: **plan**, nothing implemented yet. Goal: PhoneBorg deploys an image
generation engine to phones automatically, the same way it deploys
llama.cpp today, and serves it behind the gateway as
`POST /v1/images/generations`, with an "Images" view in the web panel.

### Summary

- Engine: [stable-diffusion.cpp](https://github.com/leejet/stable-diffusion.cpp)
  (ggml, same family as llama.cpp): static arm64 build, CPU only, GGUF and
  quantized weights, no NSFW safety checker (that is a separate diffusers
  component it does not ship).
- Models: SD-Turbo (1–4 steps) as the default candidate, SD 1.5 and its
  community fine-tunes as the second. SDXL/Flux do not fit 6–8 GB phones.
- One engine per phone at a time (today's "one model per phone" rule): a
  phone serves either text (llama-server) or images (sd-server), switched by
  placement, without re-provisioning.
- Expected speed (to be measured in phase 0): SD-Turbo 512×512 in roughly
  20–60 s on the POCO F3 (SD870), 2–3× slower on the Mi 8 (SD845).

### Phase 0: proof of concept (manual, no PhoneBorg changes)

Decide go/no-go with real numbers before touching the code.

1. Build stable-diffusion.cpp statically for arm64 in Docker, modelled on
   `runtime/llama/Dockerfile` (alpine, musl, `-static`, `GGML_NATIVE=OFF`,
   `GGML_CPU_ARM_ARCH`, `GGML_OPENMP=OFF`). Pin a release tag. Build the CLI
   (`sd`) and the HTTP server example, if the pinned tag has one; note its
   API (endpoints, request fields, whether it reports progress).
2. Get models: SD-Turbo and one SD 1.5 checkpoint as GGUF (q8_0 and q4_0),
   converting with `sd --convert` if no GGUF is published.
3. On each phone, over adb, with nothing else loaded: generate 512×512 with
   SD-Turbo (1 and 4 steps) and SD 1.5 (20 steps), 3 runs each, 4/6/8
   threads.
4. Record per run: wall time, peak RSS (`/proc/<pid>/status` VmHWM), CPU
   temperature before/after, throttling, output quality. Put the table in
   `docs/BENCHMARKS.md`.

Go criteria: SD-Turbo 512×512 ≤ 60 s on the POCO F3, peak RSS inside the
agent's memory budget (Mi 8 ~2.5 GiB, POCO ~3.3 GiB today), no thermal
shutdown over 10 consecutive images.

### Phase 1: build and provisioning (auto-deploy of the engine)

- `runtime/sd/Dockerfile` and Makefile targets `sd` / `sd-all`, producing
  `bin/sd/<ARM_ARCH>/{sd,sd-server}` for the same variants as llama.cpp
  (ADR-007), so pcprov's variant selection applies unchanged.
- pcprov `push-runtime`: push the sd binaries next to llama-server when
  `bin/sd` exists (generalise `selectLlamaServer` to pick a variant per
  engine). Binaries are always pushed, so the controller can later switch any
  phone to images without re-provisioning. Missing `bin/sd` = text only,
  today's behaviour.
- Verify: `pcprov provision` on both phones logs the sd variant, and the
  binaries are in `/data/local/tmp/phoneborg/bin/` and run (`sd --help`).

### Phase 2: node agent runtime

- `proto.DesiredRuntime` gains `Engine` (`"llama"` default, `"sd"`);
  `RuntimeStatus.Engine` already exists and reports it.
- node-agent: a second runtime next to `Runtime` in `runtime.go`, starting
  `sd-server` with the model on the serving port, health check, restart on
  crash, same model download path (`/v1/model-files/<id>`, SHA-256 check).
- Memory sizing (`sizing.go`): image estimate = weights + activation
  working set for the requested resolution (fit from phase 0 numbers), not
  the KV-cache formula.
- Benchmark on start: time one small image (e.g. 256×256, 1 step) and report
  it in place of tok/s, so the panel and planner have a speed figure.
- Verify: unit tests for engine selection and sizing; on a phone, setting a
  desired sd runtime starts sd-server and reports `ready`.

### Phase 3: controller catalog and placement

- Catalog `Model` gains `Kind` (`"text"` default, `"image"`), detected from
  GGUF metadata where possible, else set on `pbctl models add ... -kind
  image`. Image models get their own RAM estimate and `fits` classes.
- Planner: image models are placed only by explicit policy (`pin` /
  `replicas`), never as the default model; a phone runs one engine at a
  time. `placement preview` shows the switch before applying it.
- Verify: planner tests (image model never becomes default, fits check,
  pin moves a phone from text to image and back).

### Phase 4: gateway API

- `POST /v1/images/generations`, OpenAI-compatible: `model`, `prompt`, `n`,
  `size`, `response_format` (`b64_json`; `url` later). PhoneBorg extensions:
  `negative_prompt`, `steps`, `cfg_scale`, `seed`, `sampler`.
- Routing to ready nodes serving that image model; same auth and access
  modes as `/v1/*` (ADR-017); one generation per phone at a time, extra
  requests queue briefly or get 429 with `Retry-After`; long upstream
  timeout; failover only before the phone has started generating.
- `POST /admin/images/generations` for the panel, admin-token auth, usage
  as `panel` (the ADR-018 pattern).
- Metrics: `phoneborg_image_requests_total`, generation seconds histogram,
  per node; Grafana row (edit `deploy/grafana/gen_dashboard.py`).
- `/v1/models` lists image models with `kind: "image"`.
- Verify: gateway tests with a fake sd-server backend (auth, routing, busy
  phone, timeout), and a real `curl` on the cluster.

### Phase 5: web panel and pbctl

- "Images" view: model, prompt, negative prompt, size, steps, CFG, seed;
  elapsed-time indicator (and progress, if sd-server reports it); result
  with download; recent images kept per browser (IndexedDB, not
  localStorage, because of size); settings shown under each image for
  reproducibility.
- Models and Placement views show the image kind; Nodes shows the engine.
- pbctl: `models add -kind image`, engine column in `nodes`.

### Phase 6: operations

- Thermal: image generation is sustained full CPU load. Honour
  `-thermal-limit-c` before starting a job, cap steps/size per node class,
  and consider a cooldown between jobs on hot phones.
- e2e: a smoke test on emulated phones (redroid) with the smallest model and
  1 step, to keep the path tested without real phones.
- Docs: ADR-020 (engine choice, one-engine-per-phone, API shape),
  OPERATIONS.md (API, panel, pbctl), REAL_PHONES.md (expected speed), README
  feature list.

### Open questions

- **Licences:** SD-Turbo ships under Stability AI's non-commercial
  community licence; SD 1.5 under CreativeML OpenRAIL-M. Check before
  making one the default.
- **Uncensored models:** stable-diffusion.cpp has no content filter, so the
  output depends only on the model. Whoever runs the cluster is responsible
  for what it generates; decide whether the panel needs a note or a per-key
  switch.
- **Which phone:** with two phones, dedicating the POCO F3 to images leaves
  the Mi 8 alone for text. A third phone makes this much more comfortable.
- **Server API of stable-diffusion.cpp:** if the pinned tag has no usable
  HTTP server, the agent wraps the `sd` CLI (one process per image) instead,
  at the cost of reloading the model per request.

## 2. Laya: smart routing for the phone cluster

### Context

The user asked what the Laya project (github.com/NandhaKishorM/laya, Apache 2.0)
could do for the cluster. Laya is not a chat LLM: it is a small encoder
"decision model" (ModernBERT/mmBERT, 322–421M params) that answers typed
questions about a text (`choice`, `score`, `noul` = yes-probability) in one
forward pass, in 100+ languages (Polish included via `laya-multilingual`).
It ships a Python package with an HTTP server (`laya[serve]`, `laya-serve`) and
an ONNX INT8 CPU path (`laya[onnx]`). It does not run in llama.cpp, so it
does not belong on the phones.

Best fit: run it on the host (VM 10.10.100.81) next to the controller as a
fast classifier, and let the gateway use it to pick the right phone/model per
request. Today `auto` only picks by load/speed (controller/gateway/targets.go
`Resolve`, pickers in proxy.go); it knows nothing about what the request is.
With two very different phones (POCO: strong, Qwen3-4B with thinking ~7 tok/s;
Mi 8: gemma-3n ~4.6 tok/s) and thinking models that burn a minute on easy
questions, choosing model + thinking per request is the biggest win.

Uses, in priority order:
1. **Smart routing** — new virtual model `smart`: classify the last user
   message (task type, difficulty, needs reasoning, language) and send it to a
   pool/model per a configurable table.
2. **Automatic thinking on/off** — if "needs step-by-step reasoning?" is
   low, add `chat_template_kwargs.enable_thinking=false` (verified to work
   through the gateway on Qwen3).
3. **Guard for uncensored models** (optional, later) — per-API-key yes/no
   policy check before a request reaches an abliterated model.

### Phase 0 — PoC on the VM (no PhoneBorg code)

1. On the VM: `python3 -m venv /srv/phoneborg/laya` (LVM volume), install
   `laya[serve,onnx]` (CPU; avoid pulling CUDA torch — use the CPU wheel
   index per Laya's install docs), start `laya-serve` on 127.0.0.1 only.
2. Export/obtain `laya-multilingual` ONNX INT8 (`scripts/export_onnx.py
   --quantize` if no published INT8), compare with the torch CPU path.
3. Read the server API (endpoints, request/response JSON, `/health`,
   503 + Retry-After) from the installed package / docs.
4. Measure on the VM (4 vCPU, 3.8 GB RAM): RAM footprint, p50/p95 latency for
   one request with 3 questions on short PL and EN prompts; accuracy on ~20
   hand-labelled prompts (easy/hard, code/math/chat/translate, PL/EN).
5. Go criteria: p95 ≤ 150 ms, RSS ≤ 1 GB, sensible labels on ≥ 16/20.
   Record results in docs/BENCHMARKS.md.

### Phase 1 — gateway integration (if Phase 0 passes)

- Controller flag `-classifier-url` (empty = off). New virtual model
  `smart` resolved in `Gateway.Resolve` (controller/gateway/targets.go) next
  to `KindAuto`.
- New `controller/gateway/classify.go`: builds the Laya request from the last
  user message (truncate to the checkpoint's token budget), calls it with a
  short timeout (~300 ms), maps answers → target via a rule table stored in
  routing.json (persisted like pools/aliases), e.g.
  `hard|reasoning → pool/smart + thinking on`, `simple → pool/fast + thinking off`,
  `code → model X`. Fail-open: any error/timeout → behave like `auto`.
- Thinking control: when the rule says off and the client did not set
  `chat_template_kwargs` itself, inject it (reuse `withModel`-style JSON
  rewrite in proxy.go).
- Response header `X-PhoneBorg-Route: <rule>` + metric
  `phoneborg_gateway_classified_total{rule}`; latency histogram for the
  classifier call.
- Admin: `GET/PUT /admin/routing/classifier` (rules), pbctl `routing
  classifier`, panel: rules table in the Pools view; Chat model list gains
  `smart`.
- systemd unit `phoneborg-laya.service` (User=dzaczek, 127.0.0.1 only).
- Tests: classify.go with a fake Laya server (rule matching, timeout →
  fallback, thinking injection only when absent), Resolve("smart").
- Docs: next free ADR (Laya as host-side classifier, fail-open), OPERATIONS.md,
  mark the item done in docs/TODO.md.

### Phase 2 — optional

- Guard for uncensored models (per-key policy, yes/no question, block or
  reroute).
- Fine-tune `laya-typed-decisions` on logged routing decisions (Laya's Kaggle
  notebook) once there is traffic.

### Critical files

- controller/gateway/targets.go (`Resolve`, target kinds)
- controller/gateway/proxy.go (`ServeChat`, `forward`, `withModel`)
- controller/ (routing.json persistence, admin handlers — follow
  chat_admin.go / devices.go patterns), controller/cmd/controller/main.go
- controller/ui/static/js/views (pools view, chat model list)

### Verification

- Phase 0: latency/RAM/accuracy table on the VM.
- Phase 1: `go test ./...`; on the cluster, send the same easy and hard PL
  prompts with `model: "smart"` from the panel Chat: easy → fast pool, no
  thinking, fast reply; hard → POCO with thinking; stop laya-serve → requests
  still succeed (fallback to auto), header shows `fallback`.
