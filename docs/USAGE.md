# Using the cluster

This guide covers using a running PhoneBorg cluster: sending requests,
managing API keys, draining phones for maintenance and reading usage
statistics. To set up the cluster, see [REAL_PHONES.md](REAL_PHONES.md) or
[DEV_EMULATION.md](DEV_EMULATION.md).

The controller listens on `http://127.0.0.1:18080` by default:

| Path | What |
|---|---|
| `/v1/chat/completions`, `/v1/completions`, `/v1/models` | OpenAI-compatible gateway |
| `/admin/...` | admin API (needs the admin token), used by `pbctl` |
| `/` | HTML dashboard |
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

## pbctl

`pbctl` is the admin CLI. Build it with `make pbctl` (it is also part of
`make all`). It reads:

| Variable | Default | |
|---|---|---|
| `PHONEBORG_URL` | `http://127.0.0.1:18080` | controller URL (or `-url`) |
| `PHONEBORG_ADMIN_TOKEN` | none | admin token (or `-token-file FILE`) |
| `PHONEBORG_API_KEY` | none | only for `pbctl models` when keys are enforced |

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
pbctl gateway                       pbctl models
pbctl gateway set policy=affinity|least_inflight spill=<n> timeout=<duration> auth=keys
                                     thermal_limit=<celsius, 0 disables>
```

`pbctl nodes` shows a `TOK/S` column: the phone's measured generation speed
from its own self-test against llama-server (see
[Gateway settings](#gateway-settings) below for how this drives routing). A
node whose last heartbeat temperature is at or above the thermal limit shows
`HOT` next to its state and gets no new sessions.

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
phone reconnects and registers again. The dashboard shows **DRAINED**, and
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

## Grafana

`make cluster-up` provisions Grafana on http://localhost:3000 with the
PhoneBorg dashboard. It shows cluster health, latency, tokens/s, cache hit
ratio and token usage per API key over any time range. The Nodes row includes
each phone's self-test tok/s and which phones are currently hot (routed
around, ADR-010). The Administration row shows drained nodes and admin API
calls (`phoneborg_admin_actions_total`). If `result="unauthorized"` goes up,
someone is trying wrong admin tokens.
Prometheus keeps its data only for its retention period. For totals over a
longer time, use `pbctl stats` with `-state-dir`.
