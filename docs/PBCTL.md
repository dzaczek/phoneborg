# pbctl guide

> *We are PhoneBorg. Your phones will be provisioned. Your tokens will be
> streamed. Resistance is futile.*

`pbctl` is the admin CLI for the whole cluster: phones, models, pools, the
gateway, keys, jobs and agents. It talks to the controller's `/admin/*` API,
the same one the web panel uses. What each feature does is explained in
[OPERATIONS.md](OPERATIONS.md); this page is the command list, with examples.

## Contents

- [Setup](#setup)
- [Daily cheat sheet](#daily-cheat-sheet)
- [Nodes and devices](#nodes-and-devices)
- [Models and placement](#models-and-placement)
- [Pools](#pools)
- [Gateway and semantic router](#gateway-and-semantic-router)
- [Admin tokens and API keys](#admin-tokens-and-api-keys)
- [Jobs](#jobs)
- [External nodes](#external-nodes)
- [Benchmarks and statistics](#benchmarks-and-statistics)
- [Agents: MCP and opencode](#agents-mcp-and-opencode)

## Setup

| Variable | Default | What |
|---|---|---|
| `PHONEBORG_URL` | `http://127.0.0.1:18080` | controller URL (or `-url URL`) |
| `PHONEBORG_ADMIN_TOKEN` | — | admin token (or `-token-file FILE`) |
| `PHONEBORG_API_KEY` | — | API key, only for `pbctl served` when the gateway enforces keys |

On a host installed with `deploy/install.sh host`:

```sh
export PATH=$PATH:/srv/phoneborg/app/bin
export PHONEBORG_ADMIN_TOKEN=$(cat /srv/phoneborg/admin-token)
pbctl nodes
```

With the Docker install, the installer puts a `pbctl` command on the host that
runs it inside the controller container, so the same commands work.

From another machine, tunnel to the controller so it stays local:

```sh
ssh -fN -L 18080:127.0.0.1:18080 <user>@<controller-host>
PHONEBORG_ADMIN_TOKEN=$(ssh <user>@<controller-host> cat /srv/phoneborg/admin-token) pbctl nodes
```

Global flags: `-json` (raw JSON, for scripts and `jq`), `-url URL`,
`-token-file FILE`. In every command, `<id>` is a node ID, an alias, or
`ext:<name>` for an external node.

## Daily cheat sheet

| Task | Command |
|---|---|
| Is the collective healthy? | `pbctl nodes` |
| Phones waiting to be assimilated (USB, not yet authorized, failed) | `pbctl devices` |
| What can clients call? | `pbctl served` |
| Usage and cluster summary | `pbctl stats` |
| Take a phone out for maintenance | `pbctl drain <id>` … `pbctl undrain <id>` |
| Give a drone a name (`node/<alias>`) | `pbctl nodes alias <id> <alias>` |
| Add a model | `pbctl models add hf://<owner>/<repo>/<file>.gguf` |
| Where does each model run? | `pbctl placement` |
| Pools and who is eligible | `pbctl pools` |
| Running jobs | `pbctl jobs` |

## Nodes and devices

| Command | What |
|---|---|
| `pbctl nodes` | nodes, phones and external: state, drain, runtime, model, tok/s, in-flight, last seen |
| `pbctl nodes alias <id> <alias>` | name a node, addressed as `node/<alias>` (`-` clears) |
| `pbctl drain <id>` | no new requests to the node; requests already running finish |
| `pbctl undrain <id>` | put a drained node back into rotation |
| `pbctl forget <id>` | remove a node (it re-registers on its next heartbeat if alive) |
| `pbctl devices` | USB devices pcprov sees: not yet authorized, provisioning, failed |
| `pbctl devices auto on\|off` | automatic provisioning of new devices on or off |
| `pbctl devices provision <serial>` | one-shot "provision now" / "retry" for one device |
| `pbctl classes` | device classes (by RAM) and performance tiers (by GB/s) with node counts |

```sh
pbctl nodes alias 40749345 mi8
pbctl drain mi8          # finish what it has, take nothing new
pbctl undrain mi8
```

## Models and placement

| Command | What |
|---|---|
| `pbctl models` | catalog: size, status, fits, tags, serving |
| `pbctl models add <source> [-id ID] [-name NAME] [-tag a,b] [-recommend s,m]` | download a model: `hf://<owner>/<repo>/<file>.gguf`, `https://…/<file>.gguf` or `file:///abs/path.gguf` |
| `pbctl models rm <id>` | remove a model and its file |
| `pbctl models tag <id> a,b` | set tags (`-` clears); `tools` marks tool-capable models |
| `pbctl models recommend <id> classes=s,m tiers=t2,t3` | recommended device classes and tiers (`-` clears) |
| `pbctl models default <id>\|none` | model for nodes no policy claims |
| `pbctl placement` | policies, plan and each node's model state |
| `pbctl placement set <model> pin=<node,…>\|replicas=<n>\|percent=<p> [classes=s,m] [min_tok_s=<n>]` | place a model |
| `pbctl placement unset <model>` | remove the model's policy |
| `pbctl placement preview [set\|unset] [<model> …]` | show the plan a change would give, apply nothing |

```sh
pbctl models add hf://Qwen/Qwen2.5-1.5B-Instruct-GGUF/qwen2.5-1.5b-instruct-q4_k_m.gguf -tag tools
pbctl placement preview set qwen2.5-1.5b-instruct-q4_k_m replicas=2
pbctl placement set qwen2.5-1.5b-instruct-q4_k_m replicas=2 classes=m,l
pbctl placement set gemma-3n-e2b-it-q4_k_m pin=mi8
```

## Pools

| Command | What |
|---|---|
| `pbctl pools` | pools (`pool/<name>`) with member eligibility |
| `pbctl pools set <name> [models=a,b] [nodes=x,y] [classes=s,m] [min_tps=5] [routing=spread\|affinity\|superborg\|bigbrain] [orchestrator=<node>\|auto] [thinking=on\|off] [enabled=on\|off] [desc="…"] [model=<id>] [ctx=N]` | create a pool or change the given fields (`-` clears a list) |
| `pbctl pools rm <name>` | remove a pool |

```sh
pbctl pools set fast nodes=pixel,poco,mi8 desc="fast tool-less workers"
pbctl pools set writer routing=superborg nodes=oneplus,poco,pixel orchestrator=oneplus
pbctl pools set brain routing=bigbrain model=qwen3-30b-a3b-2507-ud-q3_k_xl nodes=pixel,oneplus
pbctl pools set brain enabled=off    # stop Big Brain, the phones return to their own models
```

A Super Borg pool is many phones answering as one. A Big Brain pool is many
phones thinking one thought, slowly.

## Gateway and semantic router

| Command | What |
|---|---|
| `pbctl gateway` | show gateway settings |
| `pbctl gateway set k=v …` | `policy=affinity\|least_inflight`, `spill=<n>`, `timeout=<duration>`, `first_token_timeout=<duration>`, `auth=keys`, `thermal_limit=<°C, 0 disables>`, `router=on\|off`, `router_classifier=node/<alias>`, `router_timeout=<duration>` |
| `pbctl router` | router state and classes (letter, name, target, description) |
| `pbctl router on\|off` | switch it on or off (classes and targets are kept while off) |
| `pbctl router class add <name> desc=<text> [target=<t>] [example=<text>]…` | add a class; target is `pool/<name>`, `node/<alias>`, a model id or `auto` |
| `pbctl router class set <name> [name=<new>] [desc=<text>] [target=<t>] [example=<text>]…` | change a class; given examples replace all of its examples |
| `pbctl router class rm <name>` | remove a class |
| `pbctl router classes reset` | back to the default classes |

```sh
pbctl gateway set timeout=600s first_token_timeout=30m thermal_limit=75
pbctl gateway set router=on router_classifier=node/pixel router_timeout=20s
pbctl router class set hard_coding target=pool/code
```

`thermal_limit` is the one form of resistance the collective respects: a phone
at 75 °C gets no new requests until it cools down.

## Admin tokens and API keys

| Command | What |
|---|---|
| `pbctl admin-tokens` | named admin tokens, one per operator or device |
| `pbctl admin-tokens create <name>` | create one (printed once); the audit log names it |
| `pbctl admin-tokens revoke <name>` | revoke one (`admin`, from `-admin-token-file`, cannot be) |
| `pbctl keys` | API keys with usage |
| `pbctl keys create <name>` | create an API key (printed once) |
| `pbctl keys revoke <name>` | revoke all keys of `<name>` |

```sh
pbctl admin-tokens create laptop
pbctl keys create opencode
pbctl gateway set auth=keys     # from now on every request needs a key
```

## Jobs

| Command | What |
|---|---|
| `pbctl jobs` | Super Borg jobs |
| `pbctl jobs new [title=T] [pool=P] <goal>` | start a job on Super Borg pool P (default: the first one) |
| `pbctl jobs show <id>` | tasks, documents and the latest events |
| `pbctl jobs say <id> <text>` | send an instruction (resumes a waiting, done or cancelled job) |
| `pbctl jobs doc <id> <name>` | print one document |
| `pbctl jobs result <id>` | print the assembled result (Markdown) |
| `pbctl jobs cancel\|rm <id>` | stop a job (resumable) or delete it |

```sh
pbctl jobs new title=story pool=writer "A 12-chapter story about a lighthouse keeper"
pbctl jobs show <id>
pbctl jobs say <id> "make chapter 3 shorter"
pbctl jobs result <id> > story.md
```

## External nodes

| Command | What |
|---|---|
| `pbctl external` | external engine nodes (LM Studio, oMLX, Ollama, llama-server) |
| `pbctl external add <name> <url> [key-file=path] [models=a,b] [concurrency=N] [ctx=N] [speed=N] [role=orchestrator\|worker]` | add or replace one, addressed as `node/<name>` (`key-file=-` removes the key) |
| `pbctl external rm <name>` | remove it |
| `pbctl external selftest <name>` | measure each of its models' tok/s |

```sh
pbctl external add mac http://<mac-host>:1234 models=qwen2.5-coder-14b-instruct ctx=32768
pbctl external add brain http://<desktop-host>:11434 models=qwen3:30b-a3b role=orchestrator
pbctl external selftest mac
```

Even the desktop gets assimilated: behind the gateway it is just another node.

## Benchmarks and statistics

| Command | What |
|---|---|
| `pbctl stats` | usage statistics and cluster summary |
| `pbctl served` | models ready nodes serve, pools and nodes (`/v1/models`) |
| `pbctl mmb` | multi-model benchmark runs |
| `pbctl mmb run [nodes=a,b] [pool=P] [models=x,y] [parallel]` | load every model that fits (or the given ones) on each phone and time it; phones one by one, or all at once with `parallel` |
| `pbctl mmb show\|cancel\|rm <id>` | results table / stop (phones get their models back) / delete |

```sh
pbctl mmb run nodes=pixel,poco models=qwen3-4b-instruct-2507-q4_k_m
pbctl mmb show <id>
pbctl -json stats | jq .
```

## Agents: MCP and opencode

| Command | What |
|---|---|
| `pbctl mcp` | serve the cluster as MCP tools on stdin/stdout |
| `pbctl opencode init [-dir D] [-provider P] [-base-url URL] [-force] [-read-tools] [-small-model]` | generate `opencode.json` (provider, MCP server) and `.opencode/agent` subagents |
| `pbctl opencode sync [-dir D]` | regenerate agents and config from the current cluster (idempotent) |
| `pbctl opencode watch [-dir D] [-interval 15s]` | loop `sync` until interrupted, logging changes only |
| `pbctl opencode status [-dir D]` | managed agents vs. cluster state |
| `pbctl opencode prewarm [-dir D]` | warm each managed agent's prompt cache |

```sh
cd ~/code/myproject
pbctl opencode init
pbctl opencode prewarm
opencode                 # the main model plans, the collective does the pieces
```

---

*Your phones have been assimilated. Their tokens will be added to our own.*
