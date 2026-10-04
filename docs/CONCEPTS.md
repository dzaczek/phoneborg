# Ways to use PhoneBorg

PhoneBorg can be used in several ways, from "one request, one phone" to an
agent that plans a long task and spreads it over the cluster. This page
explains each way, what it is good for, what it costs and how they combine.
Commands and options are in [OPERATIONS.md](OPERATIONS.md), design reasons in
[DECISIONS.md](DECISIONS.md), numbers in [BENCHMARKS.md](BENCHMARKS.md).

Numbers quoted here were measured on the four-phone cluster in 2026-10
(OnePlus 10 Pro, POCO F3, Pixel 8 Pro, Mi 8; see
[BENCHMARKS.md](BENCHMARKS.md#multi-model-benchmark-four-phones)).

## Contents

- [The building blocks](#the-building-blocks)
- [1. A model, `auto` or a phone](#1-a-model-auto-or-a-phone)
- [2. Pools](#2-pools)
- [3. Super Borg pool with a phone as orchestrator](#3-super-borg-pool-with-a-phone-as-orchestrator)
- [4. Super Borg pool with an external orchestrator](#4-super-borg-pool-with-an-external-orchestrator)
- [5. Jobs](#5-jobs)
- [6. An agent drives the cluster (opencode + MCP)](#6-an-agent-drives-the-cluster-opencode--mcp)
- [7. Benchmarks to choose models and phones](#7-benchmarks-to-choose-models-and-phones)
- [Comparison](#comparison)
- [Which one should I use?](#which-one-should-i-use)
- [Splitting a fleet](#splitting-a-fleet)
- [Limits that apply to all of them](#limits-that-apply-to-all-of-them)

## The building blocks

| Block | What it is |
|---|---|
| **Placement** | decides which model each phone loads (pins, replicas, percent, default). Nothing else loads models. |
| **Target** | the `model` of a request: a model id, `auto`, `pool/<name>` or `node/<alias>`. |
| **Pool** | a named set of phones (by alias, model, class, speed) that requests can address. Pools do not load models; they take the phones that already serve an allowed model. Each pool can be enabled or disabled. |
| **Super Borg pool** | a pool with routing `superborg`: one member (or an external node) orchestrates, the others are workers. |
| **Orchestrator** | the model that plans and delegates: a phone of the pool (*internal*) or an external node with `role=orchestrator` (*external*). |
| **Job** | long, multi-step work run in the background on a Super Borg pool, with a task list and a workspace of documents kept by the controller. |
| **MCP tools** | `pbctl mcp`: lets an agent (opencode) use the cluster as tools: ask, map tasks in parallel, vote, run and follow jobs. |
| **MMB** | the multi-model benchmark: loads every model that fits on chosen phones and measures them the same way. |

## 1. A model, `auto` or a phone

One request goes to one phone. `auto` and a model id let the gateway choose
(least busy, session affinity, cool phones first); `node/<alias>` forces a
phone.

**Good for:** chat, single questions, an OpenAI or Ollama client that just
needs "a model", tests of one phone.

| Pros | Cons |
|---|---|
| simplest; works with any OpenAI/Ollama client | one phone per request: a long answer runs at that phone's speed |
| session affinity keeps a conversation on the phone that has its prompt cache (warm answers in about 1 s) | with mixed models, `auto` may land on a weak one |
| failover to another phone if one dies before answering | |

## 2. Pools

A pool groups phones for one purpose, e.g. `pool/fast` (any phone, small
models), `pool/smart` (only 4B+ models), `pool/code` (models good at code and
tools). Routing is `spread` (least busy, no session pinning: best for many
short parallel requests) or `affinity` (conversations stick to a phone).

**Good for:** agents with several subagents (opencode), separating kinds of
work, giving clients a stable name while phones and models change behind it.

| Pros | Cons |
|---|---|
| parallelism: independent requests run on different phones at once | a pool is only as good as the phones that currently serve its models: when placement changes, it can end up with no ready phone (its status says why) |
| quality control: a pool can exclude weak models | pools do not split one request; that needs Super Borg or an agent |
| requests with tools go only to tool-capable models in the pool (ADR-023) | |
| conflicting enabled pools are refused (ADR-027) | |

## 3. Super Borg pool with a phone as orchestrator

A chat request to a Super Borg pool goes to its orchestrator, the member
with the best model (e.g. Qwen3-8B on the OnePlus). It answers simple
questions itself, or calls `delegate` to run sub-tasks on the other members
in parallel, then writes the final answer from their results (at most two
delegation rounds).

**Good for:** "one smart answer" from a self-contained cluster, a few
minutes long, with parts that can be done in parallel; experiments; offline
use.

| Pros | Cons |
|---|---|
| fully local, nothing leaves the network, no cost per token | the orchestrator is the bottleneck: Qwen3-8B on a phone processes prompts at ~6 tok/s and generates ~3–4 tok/s |
| one request, one answer: works from any client and from the panel's Chat | measured: a three-part request took ~2 min with a 4B orchestrator and ~6 min with the 8B one |
| workers run in parallel | small orchestrators plan weakly: they sent dependent steps in parallel and passed on wrong worker answers until the prompt rules were tightened (ADR-020) |
| | a chat answer cannot hold long work (a 20-chapter story is more than the orchestrator's 16k context) |

## 4. Super Borg pool with an external orchestrator

The same loop, but the orchestrator is an external node with
`role=orchestrator` (ADR-029): Ollama or oMLX on a desktop, LM Studio, or a
hosted API such as DeepSeek. The phones stay the workers. The external node
gets no other traffic and is never chosen automatically.

**Good for:** the same tasks as 3, when planning quality and speed matter.

| Pros | Cons |
|---|---|
| a much stronger planner: a desktop or API model processes the prompt in seconds instead of minutes and follows the delegation rules far better | needs that server up and reachable (a desktop server must listen on the LAN, not 127.0.0.1) |
| the phones still do the work in parallel | a hosted API costs money per token and sees the prompts (do not send private data) |
| falls back to the pool's best phone when the external node is down | one more moving part: its own keys, limits and outages |
| no other traffic can reach it, so a paid API is only used for orchestration | |

## 5. Jobs

A job (ADR-021) runs long work in the background: a story in chapters, a set
of documents, many similar items. The controller keeps the job's task list,
documents and log; each orchestrator step is a fresh, small request that
must call one tool (`plan_tasks`, `write_doc`, `delegate`, `read_doc`,
`ask_user`, `finish`). The gateway sends delegated tasks to the fastest free
workers, several at once. Jobs survive restarts, can be steered with
messages while they run, and their result is assembled from the documents.

A job runs on a Super Borg pool, so it uses that pool's orchestrator:

| | Internal orchestrator (a phone) | External orchestrator (desktop/API) | No Super Borg pool |
|---|---|---|---|
| Who plans | the pool's best phone (e.g. Qwen3-8B) | the external node | the best tool-capable phone of the cluster |
| Step time (measured) | 5–10 min per orchestrator step with the 8B on a phone | seconds to tens of seconds | as internal |
| Planning quality | needed guards: repeated plans, English instead of Polish, stopping after 1 of 20 chapters (all fixed in code, ADR-021) | much better | as internal |
| Cost | none | per token for an API | none |
| Typical result | a 12-document story in ~70 min with three phones writing in parallel | the same writing time on the phones, far less planning time | |

| Pros | Cons |
|---|---|
| no lost work: every result is a document, kept across restarts | slow: plan for an hour or more for long texts on phones |
| the orchestrator's context stays small however long the job | the writing quality is that of the small worker models |
| steerable: send a message and the next step sees it | chapters written in parallel see the outline, not each other, so transitions are weaker than when written one by one |
| fair use of phones: the fastest free phone gets the next task | one job runs at a time |

## 6. An agent drives the cluster (opencode + MCP)

A capable main model in opencode (an API model, or a desktop model through
LM Studio/oMLX) plans the work, edits files and runs tests; the phones do
pieces through the MCP server `pbctl mcp` and the generated subagents:

- `cluster_map`: independent pieces at once (tests for six functions,
  summaries of ten files);
- `cluster_vote`: several phones judge one thing, with a threshold such as
  `4/5` (a quality gate);
- `job_start` / `job_wait` / `job_result`: long writing handed to a job and
  followed without polling;
- `@code-review`, `@code-tests`, `@code-docs` (on `pool/code`), `@borg-*`,
  `@phone-*`.

The generated instructions (`pbctl opencode init`, also `-global`) teach the
agent the loop: cut the task into small pieces, map them, check the results
with tests or votes, resend only what failed, apply every edit itself.

| Pros | Cons |
|---|---|
| the best planner and the phones' parallelism together | needs a strong main model; with a phone as main model the loop is slow and weak (opencode's own prompt is ~11.7k tokens: ~30 min of prompt processing at 6 tok/s) |
| works for programming: the agent keeps the repository, the phones write and review small, self-contained pieces | phones see only what is sent and have a 16k context: no repository-wide work on the phones |
| votes give a cheap second opinion | the agent must check everything; small models are often wrong |
| | opencode does not wait in the background: a long job is followed with `job_wait` or by asking later |

## 7. Benchmarks to choose models and phones

MMB (ADR-028) loads every model that fits on the chosen phones (one by one,
or several phones at the same time) and measures load time, time to first
token with and without a cached prefix, prompt and generation speed, and a
short answer. The panel shows a phones × models table per parameter and a
legend of what each one means.

**Use it** before choosing placement, when a phone or model is added, or to
check a suspected throttling or memory problem. A benchmark takes phones out
of service for minutes per model.

## Comparison

| Way | Who plans | Parallel | Latency for a short task | Long tasks | Cost | Setup |
|---|---|---|---|---|---|---|
| 1. model / `auto` / node | nobody | across requests | seconds (warm) | no | none | none |
| 2. pools | the client | across requests | seconds | no | none | pools |
| 3. Super Borg, phone orchestrator | a phone | inside one request | minutes | no (one answer) | none | a pool |
| 4. Super Borg, external orchestrator | desktop/API | inside one request | tens of seconds to minutes | no (one answer) | per token for an API | a pool + an external node |
| 5. jobs | pool's orchestrator | tasks on several phones | minutes per step | **yes** (hours) | as 3 or 4 | a pool (optional) |
| 6. agent + MCP | the agent's model | `cluster_map`, subagents, jobs | depends on the agent | yes, through jobs | the agent's model | `pbctl opencode init`, a strong main model |

## Which one should I use?

| I want… | Use |
|---|---|
| a chat model for an app or an OpenAI/Ollama client | 1 (`auto` or a pool) |
| several opencode subagents at once | 2 (pools) or 6 |
| one good answer that combines a few parts, fully local | 3 |
| the same, better and faster, and I have a desktop or an API key | 4 |
| a long text in many parts (a story, a report) | 5, with an external orchestrator if available |
| programming help with the phones as helpers | 6 with `pool/code` |
| to know which model to put on which phone | 7 (MMB) |

## Splitting a fleet

All of these can run at the same time. With many phones, split them by
pool membership, e.g. 100 phones:

```sh
pbctl pools set borg   routing=superborg nodes=<50 phones> orchestrator=desktop   # jobs and Super Borg answers
pbctl pools set code   nodes=<30 phones> models=qwen3-4b-instruct-2507-q4_k_m       # opencode coding helpers
pbctl pools set fast   nodes=<20 phones>                                         # chat and small subagents
```

Pools that name the same phone must share a model, otherwise saving or
enabling the second one is refused (ADR-027). Placement still decides the
models; give the phones of each pool a policy that matches the pool.

## Limits that apply to all of them

- **Speed is memory bandwidth.** A 4B model generates ~5 tok/s and an 8B one
  ~3 tok/s on these phones; prompt processing is 3–10× faster than
  generation but still slow for long prompts.
- **Context is 16k tokens per phone.** Long inputs must be cut down.
- **Prompt cache works only for some architectures.** Qwen, Llama, Phi and
  SmolLM answer a follow-up in about 1 s; Gemma, LFM2 and Granite-H
  re-process the whole prompt every time (see
  [BENCHMARKS.md](BENCHMARKS.md#multi-model-benchmark-four-phones)).
- **Heat.** A hot phone throttles hard: at 79 °C the OnePlus's measured
  bandwidth dropped so far that the planner predicted ~1 tok/s for Qwen3-8B
  instead of ~3.7 when cool. Long benchmarks and jobs heat phones up.
- **Small models make mistakes.** Every way above needs checking: by the
  orchestrator, by votes, by tests, or by you.
