# Using the cluster

This guide covers using a running PhoneBorg cluster: sending requests,
managing API keys, choosing which models phones serve, draining phones for
maintenance and reading usage statistics. To set up the cluster, see [REAL_PHONES.md](REAL_PHONES.md) or
[DEV_EMULATION.md](DEV_EMULATION.md).

The controller listens on `http://127.0.0.1:18080` by default:

| Path | What |
|---|---|
| `/v1/chat/completions`, `/v1/completions`, `/v1/models` | OpenAI-compatible gateway |
| `/admin/...` | admin API (needs the admin token), used by `pbctl` and the web panel |
| `/v1/model-files/<model_id>` | model downloads for phones (see [Models](#models-and-placement)) |
| `/ui/` | [web management panel](#web-panel) (`/` redirects here) |
| `/status` | read-only HTML table of nodes, no login |
| `/metrics` | Prometheus metrics (`phoneborg_*`) |

## Sending requests

With curl:

```sh
curl http://127.0.0.1:18080/v1/chat/completions \
  -H "Authorization: Bearer $PHONEBORG_API_KEY" \
  -d '{"model":"qwen2.5-0.5b-instruct-q4_k_m",
       "messages":[{"role":"user","content":"Hello from a phone cluster!"}]}'
```

Leave out the `Authorization` header while the gateway is open (see
[API keys](#api-keys)). Omitting `model` picks any ready phone. The
`X-PhoneBorg-Node` response header says which phone answered.

With the OpenAI Python SDK:

```python
from openai import OpenAI

client = OpenAI(base_url="http://127.0.0.1:18080/v1", api_key="pb-...")  # any string while the gateway is open
resp = client.chat.completions.create(
    model="qwen2.5-0.5b-instruct-q4_k_m",
    messages=[{"role": "user", "content": "Write a haiku about old phones."}],
    stream=True,
)
for chunk in resp:
    print(chunk.choices[0].delta.content or "", end="", flush=True)
```

With opencode, add a provider entry in `~/.config/opencode/opencode.jsonc`,
under `"provider"`. Phones need a large context for opencode; see
[DEV_EMULATION.md](DEV_EMULATION.md#using-the-cluster-from-opencode).

```jsonc
"phoneborg": {
  "npm": "@ai-sdk/openai-compatible",
  "name": "PhoneBorg (phones)",
  "options": { "baseURL": "http://127.0.0.1:18080/v1", "apiKey": "{env:PHONEBORG_API_KEY}",
               "timeout": false, "headerTimeout": 1800000, "chunkTimeout": 1800000 },
  "models": { "qwen2.5-0.5b-instruct-q4_k_m": {
    "name": "Qwen2.5 0.5B on phones", "limit": { "context": 16384, "output": 2048 } } }
}
```

Requests that share a system prompt and tools stay on one phone (session
affinity), so follow-up turns reuse its prompt cache.

## Web panel

Open http://127.0.0.1:18080/ (it redirects to `/ui/`). The panel needs the
admin API, so start the controller with `-admin-token-file` (see
[pbctl](#pbctl) below for creating the token). Sign in with that token. It
is kept for this browser tab only (session storage) and sent as a bearer
header; **Log out** forgets it. The panel is built into the controller and
loads nothing from the internet. There is no TLS yet, so use it on
localhost or a trusted network.

| View | What it does |
|---|---|
| Overview | Nodes by state, ready, drained and hot nodes, requests/s and tokens/s (last 30 s, measured in the browser), errors, models served, placement warnings. |
| Nodes | Every node with device, class, state (DRAINED and HOT badges), model and build, threads, context, measured tok/s, RAM, temperature, battery, in-flight requests, pinned sessions and last heartbeat. Drain, undrain and forget (with confirmation). Select a node id for its inventory and runtime details. |
| Models | The model catalog: download status, size, estimated RAM, which device classes it fits, tags. Add a model from `https://…`, `hf://owner/repo/file.gguf` or `file:///path`, edit tags, recommended classes and the default flag, delete (the reason is shown if the controller refuses). |
| Placement | Device classes, and policies per model: pin to nodes, a number of replicas, or a percentage of eligible nodes (shows the resulting node count as you move the slider), optionally only on some classes; the default model. **Preview** shows which nodes would change model, with RAM estimates and warnings; **Apply** is enabled only after a preview of the current edits. The current plan table shows each node's current and target model and download progress. |
| Proxy | Gateway settings: routing policy, affinity spill, upstream timeout, thermal limit, and enforcing API keys (one-way, with confirmation). Changes apply at once and are not saved across restarts. |
| API keys | Keys with their usage. Create a key (shown once, with a copy button) and revoke keys. |
| Usage | Requests, errors, prompt, cached and completion tokens, average tok/s and last use, per key and per node, since start or since first use (with `-state-dir`). |

Live views refresh every 5 s. **Settings** sets the Grafana and Prometheus
links (by default ports 3000 and 9090 on the controller's host); you can also
open `/ui/?grafana=URL&prometheus=URL` once to set them. Models and
Placement need a controller with the model management API; otherwise they
say so and the other views work as usual.

`/status` keeps the old plain table, without login, for a quick look.

## pbctl

`pbctl` is the admin CLI. Build it with `make pbctl` (it is also part of
`make all`). It reads:

| Variable | Default | |
|---|---|---|
| `PHONEBORG_URL` | `http://127.0.0.1:18080` | controller URL (or `-url`) |
| `PHONEBORG_ADMIN_TOKEN` | none | admin token (or `-token-file FILE`) |
| `PHONEBORG_API_KEY` | none | only for `pbctl served` when keys are enforced |

The admin API is off unless the controller runs with
`-admin-token-file FILE`. The file holds one token of at least 16
characters (`#` comment lines are allowed). The docker compose stack uses
`deploy/dev-admin-token`. That token is public, so use it only for local
development:

```sh
export PHONEBORG_ADMIN_TOKEN=$(grep -v '^#' deploy/dev-admin-token)
bin/pbctl nodes
```

For your own controller:

```sh
openssl rand -hex 32 > admin-token && chmod 600 admin-token
bin/controller -admin-token-file admin-token -api-keys-file api-keys -state-dir state/
bin/pbctl -token-file admin-token nodes
```

Commands print tables. Add `-json` to get the raw API response.

```text
pbctl nodes                         pbctl keys
pbctl drain <id>                    pbctl keys create <name>
pbctl undrain <id>                  pbctl keys revoke <name>
pbctl forget <id>                   pbctl stats
pbctl gateway                       pbctl served
pbctl gateway set policy=affinity|least_inflight spill=<n> timeout=<duration> auth=keys
                                     thermal_limit=<celsius, 0 disables>
pbctl models                        pbctl classes
pbctl models add <source> [-id ID] [-name NAME] [-tag a,b] [-recommend s,m]
pbctl models rm <id>                pbctl models tag <id> a,b
pbctl models recommend <id> s,m     pbctl models default <id>|none
pbctl placement                     pbctl placement unset <model>
pbctl placement set <model> pin=<node,...>|replicas=<n>|percent=<p> [classes=s,m]
pbctl placement preview [set|unset] [<model> ...]
```

`pbctl served` lists the models that ready phones serve right now (the
gateway's `/v1/models`). It was called `pbctl models` before the model
catalog existed; `pbctl models` now manages the catalog (see
[Models and placement](#models-and-placement)).

`pbctl nodes` shows a `TOK/S` column: the phone's measured generation speed
from its own self-test against llama-server (see
[Gateway settings](#gateway-settings) below for how this drives routing). A
node whose last heartbeat temperature is at or above the thermal limit shows
`HOT` next to its state and gets no new sessions.

## Models and placement

Phones provisioned with `pcprov -model FILE` serve that file until you say
otherwise. The controller can also keep a **catalog** of GGUF models and
decide which phone serves which model. Phones then download the model from
the controller over their USB link and switch to it; nobody pushes files by
hand. See ADR-011 in [DECISIONS.md](DECISIONS.md).

### Adding models

```sh
pbctl models add hf://Qwen/Qwen2.5-1.5B-Instruct-GGUF/qwen2.5-1.5b-instruct-q4_k_m.gguf -tag chat -recommend m,l
pbctl models                  # progress, then size, quantization, fits, tags
```

A source is one of:

- `hf://<owner>/<repo>/<file>.gguf`, fetched from
  `https://huggingface.co/<owner>/<repo>/resolve/main/<file>.gguf`;
- any `https://` URL of a `.gguf` file;
- `file:///absolute/path.gguf` on the controller's machine (in the compose
  stack that is inside the container, so use `hf://` or `https://` there).

The controller downloads the file into `-models-dir` (default
`<state-dir>/models`; in the compose stack that is the `controller-state`
volume). It resumes an interrupted download, computes the SHA-256, reads the
GGUF metadata (architecture, parameter count, quantization, trained context,
layers, KV heads) and only then marks the model `ready`. A file that is not
a valid GGUF (for example an HTML login page) ends as `error` and is
deleted. Adding the same id again retries a failed download. Split GGUF
files (`-00001-of-0000N.gguf`) and gated repositories that need a token are
not supported.

The model id is the file name without `.gguf`, lowercased, so
`Qwen2.5-0.5B-Instruct-Q4_K_M.gguf` becomes
`qwen2.5-0.5b-instruct-q4_k_m`, the same name the phones already serve when
provisioned with that file. Use `-id` to choose another. The id is also the
name clients put in `"model"`.

Models that suit phones (Q4 quantizations of 1–4B models). These are real
Hugging Face repositories, but file names change between uploads: **verify
the file name on Hugging Face** before adding.

| Model | Source |
|---|---|
| Qwen2.5 1.5B Instruct | `hf://Qwen/Qwen2.5-1.5B-Instruct-GGUF/qwen2.5-1.5b-instruct-q4_k_m.gguf` |
| Gemma 3 1B it | `hf://bartowski/google_gemma-3-1b-it-GGUF/google_gemma-3-1b-it-Q4_K_M.gguf` |
| Llama 3.2 1B Instruct | `hf://bartowski/Llama-3.2-1B-Instruct-GGUF/Llama-3.2-1B-Instruct-Q4_K_M.gguf` |
| Llama 3.2 3B Instruct | `hf://bartowski/Llama-3.2-3B-Instruct-GGUF/Llama-3.2-3B-Instruct-Q4_K_M.gguf` |
| DeepSeek-R1-Distill-Qwen 1.5B | `hf://bartowski/DeepSeek-R1-Distill-Qwen-1.5B-GGUF/DeepSeek-R1-Distill-Qwen-1.5B-Q4_K_M.gguf` |
| Phi-4-mini Instruct (3.8B) | `hf://bartowski/microsoft_Phi-4-mini-instruct-GGUF/microsoft_Phi-4-mini-instruct-Q4_K_M.gguf` |

Check each model's license on its Hugging Face page (Gemma and Llama have
their own terms). `pbctl models -json` shows the license recorded in the
file, when there is one.

### Device classes and fit

Phones are grouped by total RAM:

| Class | RAM |
|---|---|
| `xs` | < 3 GiB |
| `s` | 3–5 GiB |
| `m` | 5–7 GiB |
| `l` | 7–10 GiB |
| `xl` | 10 GiB+ |

`pbctl classes` shows how many phones each class has and which models you
recommended for it (`pbctl models recommend <id> s,m`; recommendations and
tags are notes for operators and the web panel, the planner does not use
them).

For each model the controller estimates the RAM it needs with a 16k-token
context: the file size, plus an f16 KV cache for 16384 tokens (or the
model's trained context, if smaller), plus 150 MiB. A model **fits** a phone
when that estimate is at most the phone's RAM minus 2 GiB for Android.
`FITS (16k)` in `pbctl models` lists the classes where it fits on the
smallest phone. The estimate is conservative: the phone's agent may lower
the context or quantize the KV cache to make a model fit.

This 2 GiB baseline is only a guess at what Android leaves free, and it is
often wrong: emulated 2–3 GiB phones use closer to 0.65 GiB, while a real
phone with more background apps can use more. Once a phone has sent one
heartbeat, placement instead checks fit against its actual reported memory
budget (the same accounting its agent uses before switching models), so a
phone with more real headroom than the 2 GiB guess assumes can get a model
the class-based `FITS (16k)` column would not show as fitting. `pbctl
placement` shows each phone's budget in the `BUDGET` column (also
`budget_bytes` in `Placement.nodes` and the web panel); `FITS (16k)` on the
catalog stays the class heuristic, since it describes a class of phones, not
one connected phone.

### Placement

Placement decides which model each phone serves. One phone serves one
model. Policies are applied in this order:

1. **pin**: these phones serve this model, even if it does not fit (you get
   a warning).
2. **replicas**: this many phones serve it.
3. **percent**: this share of the phones where it fits serves it, rounded
   half up, at least one.
4. **default model**: every phone no policy claimed, where it fits.
5. Otherwise a phone **keeps** what it serves (for example the model
   `pcprov -model` pushed).

`classes=s,m` limits replicas and percent policies to those device
classes. Bigger models choose first and get the faster phones (measured
tok/s). A phone that already serves a model stays on it rather than
switching. Drained phones only follow pins.

```sh
pbctl placement set qwen2.5-1.5b-instruct-q4_k_m replicas=2 classes=m,l
pbctl placement set llama-3.2-3b-instruct-q4_k_m pin=5f1e2d3c4b5a6978
pbctl placement set google_gemma-3-1b-it-q4_k_m percent=50
pbctl models default qwen2.5-0.5b-instruct-q4_k_m
pbctl placement preview set google_gemma-3-1b-it-q4_k_m percent=100   # what would change; applies nothing
pbctl placement                    # policies, the plan per phone, phone states, warnings
pbctl placement unset google_gemma-3-1b-it-q4_k_m
```

The plan is recomputed when you change placement, when phones join, leave
or are drained, when a model becomes ready, and every 10 seconds. Phones
learn their assignment from the reply to their next heartbeat and download
the model from `/v1/model-files/<id>`. While a phone downloads or loads a
model it gets no requests; the others keep serving. Agents from before
model management ignore the reply and keep their pushed model. A policy for a model
that is still downloading waits (with a warning) until the model is ready.
You cannot remove a model that a policy or the default uses.

Policies and the default model are saved to `<state-dir>/placement.json`,
the catalog to `<state-dir>/models.json`. Without `-state-dir` both live in
memory and model files go to a temporary directory.

`/v1/model-files/` needs no token, like the heartbeat, so anything that
reaches the controller's port can download catalog models. Keep the
controller on localhost or a trusted network (ADR-002, ADR-011).

## API keys

Each API key has an owner name. Usage is counted per name in `pbctl stats`,
`pbctl keys` and Grafana. The gateway runs in one of two modes:

- **open**: requests without a key, or with an unknown one, are served as
  `anonymous`. Requests with a known key are counted under its name. This is
  the default without `-api-keys-file`.
- **keys**: requests without a valid key get HTTP 401. This is the default
  with `-api-keys-file`.

Lifecycle:

```sh
pbctl keys create alice          # prints the key once: pb-...  store it now
pbctl keys                       # names, creation time, usage; never the keys
pbctl keys revoke alice          # takes effect immediately
```

- With `-api-keys-file`, created and revoked keys are written to that file.
  The write is atomic and the file is mode 0600. Only SHA-256 hashes are
  stored: `alice sha256:<hex> 2026-09-23T10:00:00Z`. Legacy `<name> <key>`
  lines still work. The first change made through pbctl rewrites them as
  hashes, and the keys keep working.
- Without `-api-keys-file`, keys live in memory and are lost on restart.
  pbctl prints a note when this happens.
- If you edit the file by hand, run `kill -HUP <controller pid>` to reload it.
  A broken file is rejected and the current keys stay in effect.
- An open gateway stays open when you create keys, so clients can switch to
  keys before you enforce them. Enforce keys with `pbctl gateway set auth=keys`
  once every client sends one. You cannot switch back to open at runtime:
  restart the controller without `-api-keys-file`. See ADR-008 in
  [DECISIONS.md](DECISIONS.md).

## Draining a phone for maintenance

```sh
pbctl nodes                      # find the node id (device serial or android_id)
pbctl drain 5f1e2d3c4b5a6978     # no new requests; running ones finish
pbctl nodes                      # wait until INFLIGHT is 0
# unplug, reflash, cool down, replace the battery...
pbctl undrain 5f1e2d3c4b5a6978
```

When you drain a phone, its pinned sessions move to other phones on their
next request. The drain is kept by node id, so it stays in effect when the
phone reconnects and registers again. The web panel and `/status` show **DRAINED**, and
Prometheus has `phoneborg_node_drained{node_id}`. The drain flag is kept in
memory and is lost when the controller restarts.

`pbctl forget <id>` removes a node, for example a phone that you retired. A
phone that is still running registers again on its next heartbeat. Requests
in flight on a forgotten node are retried on another phone, so drain it
first if you want them to finish.

## Gateway settings

```sh
pbctl gateway                                    # current settings
pbctl gateway set policy=least_inflight          # plain load balancing
pbctl gateway set policy=affinity spill=3        # session affinity, tolerate 3 extra requests
pbctl gateway set timeout=900s                   # applies to requests that start afterwards
pbctl gateway set thermal_limit=70               # phones at/above 70°C get no new sessions
```

Changes apply at once and are not saved. After a restart, the controller
uses its flags again (`-upstream-timeout`; the policy is `affinity` with
`spill=2`; the thermal limit is `-thermal-limit-c`, default 75°C).

A node at or above the thermal limit is routed around: it gets no new
sessions unless every ready phone is that hot, and a phone that heats up
mid-session loses its pinned sessions to a cooler one, the same way a busy
phone does. Set `thermal_limit=0` to disable this. See ADR-010 in
[DECISIONS.md](DECISIONS.md).

## Usage statistics

```sh
pbctl stats
```

`pbctl stats` shows the uptime and a cluster summary: nodes by state, drained
nodes, ready phones and models. It then shows usage by API key and by node:
requests, errors, prompt tokens, cached prompt tokens, completion tokens,
average generation speed (token-weighted) and last use.

- **Since start**: always shown.
- **Since first use**: shown only with `-state-dir DIR`. The totals are saved
  to `DIR/usage.json` every 30 s and on a clean shutdown (SIGINT or
  SIGTERM), and loaded again at startup. The compose stack keeps them in the
  `controller-state` volume. If `usage.json` is corrupt, the controller
  refuses to start. Move the file away to start from zero.

Key counts include only client requests. A failed attempt that the gateway
retries on another phone counts as an error for that node, but not as a
request for the key. Errors are all outcomes other than HTTP 200, including
disconnected clients (499).

## OpenCode agent bridge

`pbctl opencode` generates and keeps in sync an opencode provider entry and a
set of `.opencode/agent/` subagents that run on the phone cluster, instead of
hand-editing `opencode.jsonc` as in [Sending requests](#sending-requests)
above. It targets the gateway's virtual models: `auto` (any ready phone),
`pool/<name>` (a named group of phones) and `node/<alias>` (one pinned
phone).

```sh
export PHONEBORG_URL=http://127.0.0.1:18080
export PHONEBORG_ADMIN_TOKEN=$(grep -v '^#' deploy/dev-admin-token)
cd my-project
pbctl opencode init          # creates opencode.json and .opencode/agent/*.md
pbctl opencode status        # NAME | TARGET | SERVED MODEL | NODES READY | STATUS
pbctl opencode sync          # re-run after nodes join, leave or get an alias
pbctl opencode watch         # loop "sync" every 15s, logging only when something changes
pbctl opencode prewarm       # warm each managed agent's target so its first turn is fast
```

`init`:

- Creates the `fast` pool (spread routing, tool-less workers on any phone) if
  the controller doesn't have one yet; an older controller without the pools
  API is skipped with a warning, the rest of `init` still runs.
- Writes `opencode.json` with a `provider.phoneborg` block listing every
  model `GET /v1/models` reports (real models, `auto`, pools, `node/<alias>`),
  each with a context limit taken from that node's `ctx_size` when known,
  else 16384. If a project config already exists: a plain JSON file without
  `provider.phoneborg` gets the block merged in (other keys untouched, a
  `.bak` kept); a JSONC file, or one that already has the block, is left
  alone and the block is printed to paste by hand (`-force` replaces the
  block in a plain JSON file). Your global `~/.config/opencode` is never
  touched.
- Writes agents to `.opencode/agent/`: `borg-fast`, `borg-summarize` and
  `borg-review` (all `pool/fast`), plus one `phone-<alias>` per aliased node
  (`node/<alias>`, pinned, no failover). Every agent is `mode: subagent` and
  tool-less (`tools: {"*": false}`) by default; `-read-tools` allows `read`,
  `grep` and `glob` instead (warns: about 1k extra prompt tokens per call).
  Generated files end with a `<!-- generated by pbctl opencode -->` marker
  and are tracked with a content hash in `.opencode/phoneborg-managed.json`.

`sync` regenerates agents from the current cluster state: adds agents for
newly aliased nodes, removes agents for aliases that are gone, and refreshes
the provider's model list when the config is the plain-JSON file `init`
created. It is idempotent and prints a diff (`added/removed/updated/kept`).
A managed file you edited by hand is kept as is, with a warning, instead of
being overwritten or deleted. `watch` just loops `sync` on an interval and
only logs when something changed.

`prewarm` sends each managed agent's target a `POST /admin/prewarm` with its
prompt body as a system message, so llama.cpp caches the prompt prefix
before you actually use the agent. OpenCode appends its own environment
details to every system prompt regardless, so this mostly helps agents with
long prompts or tools — a stock, tool-less `borg-fast` call is already only
about 182 prompt tokens, against roughly 11.7k for opencode's default build
agent (see [Using the cluster from
opencode](DEV_EMULATION.md#using-the-cluster-from-opencode)); that gap is why
the generated agents are tool-less by default.

Once generated, call them like any other subagent, including in parallel:

```text
use @borg-review and @borg-summarize in parallel on the diff and the PR description
```

## Grafana

`make cluster-up` provisions Grafana on http://localhost:3000 with the
PhoneBorg dashboard. It shows cluster health, latency, tokens/s, cache hit
ratio and token usage per API key over any time range. The Nodes row includes
each phone's self-test tok/s and which phones are currently hot (routed
around, ADR-010). The Administration row shows drained nodes and admin API
calls (`phoneborg_admin_actions_total`). The Models row shows the catalog,
controller downloads, planned versus serving phones per model and phones
that are switching models. If `result="unauthorized"` goes up,
someone is trying wrong admin tokens.
Prometheus keeps its data only for its retention period. For totals over a
longer time, use `pbctl stats` with `-state-dir`.
