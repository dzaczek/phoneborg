#!/usr/bin/env python3
"""Generates the dashboards in deploy/grafana/dashboards/ (phoneborg.json, the
per-node phoneborg-node.json and phoneborg-tokens.json). Edit this file, not the JSON.

    python3 deploy/grafana/gen_dashboard.py
"""
import json
import os
DS = {"type": "prometheus", "uid": "prometheus"}
panels, pid, y = [], [1], [0]

def nid():
    pid[0] += 1; return pid[0]

def row(title):
    panels.append({"type": "row", "title": title, "id": nid(), "collapsed": False,
                   "gridPos": {"h": 1, "w": 24, "x": 0, "y": y[0]}, "panels": []})
    y[0] += 1

def stat(title, expr, x, w=4, unit="none", thresholds=None, desc=""):
    panels.append({"type": "stat", "title": title, "id": nid(), "datasource": DS, "description": desc,
        "gridPos": {"h": 4, "w": w, "x": x, "y": y[0]},
        "targets": [{"refId": "A", "expr": expr, "datasource": DS, "instant": True}],
        "options": {"reduceOptions": {"calcs": ["lastNotNull"]}, "colorMode": "value", "graphMode": "area"},
        "fieldConfig": {"defaults": {"unit": unit, "thresholds": {"mode": "absolute",
            "steps": thresholds or [{"color": "green", "value": None}]}}, "overrides": []}})

def ts(title, targets, x, w=12, h=8, unit="none", desc="", stack=False):
    panels.append({"type": "timeseries", "title": title, "id": nid(), "datasource": DS, "description": desc,
        "gridPos": {"h": h, "w": w, "x": x, "y": y[0]},
        "targets": [{"refId": chr(65+i), "expr": e, "legendFormat": l, "datasource": DS} for i, (e, l) in enumerate(targets)],
        "fieldConfig": {"defaults": {"unit": unit, "custom": {"lineWidth": 2, "fillOpacity": 10,
            "stacking": {"mode": "normal" if stack else "none"}}}, "overrides": []},
        "options": {"legend": {"displayMode": "table", "placement": "right", "calcs": ["lastNotNull", "max"]},
                    "tooltip": {"mode": "multi"}}})

row("Cluster")
red = [{"color": "green", "value": None}, {"color": "red", "value": 1}]
stat("Active nodes", 'phoneborg_nodes{state="ACTIVE"}', 0)
stat("Unhealthy nodes", 'sum(phoneborg_nodes{state=~"SUSPECT|OFFLINE"})', 4, thresholds=red)
stat("Models ready", 'sum(phoneborg_node_runtime_ready) or vector(0)', 8, desc="Nodes whose llama-server is loaded and healthy")
stat("Requests / s", 'sum(rate(phoneborg_gateway_requests_total[1m])) or vector(0)', 12, unit="reqps")
stat("Generated tokens / s", 'sum(rate(phoneborg_gateway_tokens_total{kind="completion"}[1m])) or vector(0)', 16)
stat("Error ratio (5m)", '(sum(rate(phoneborg_gateway_upstream_errors_total[5m])) + sum(rate(phoneborg_gateway_rejected_total[5m]))) / clamp_min(sum(rate(phoneborg_gateway_requests_total[5m])), 1e-9) or vector(0)',
     20, unit="percentunit", thresholds=[{"color": "green", "value": None}, {"color": "orange", "value": 0.01}, {"color": "red", "value": 0.05}])
y[0] += 4

row("Cluster tokens per minute")
IN, OUT = 'kind="prompt"', 'kind="completion"'
stat("Input tokens / min", f'sum(increase(phoneborg_gateway_tokens_total{{{IN}}}[1m])) or vector(0)', 0, w=6,
     desc="Prompt tokens sent to the phones in the last minute (incl. ones served from the prompt cache)")
stat("Output tokens / min", f'sum(increase(phoneborg_gateway_tokens_total{{{OUT}}}[1m])) or vector(0)', 6, w=6,
     desc="Tokens the phones generated in the last minute")
stat("Input tokens (time range)", f'sum(increase(phoneborg_gateway_tokens_total{{{IN}}}[$__range])) or vector(0)', 12, w=6)
stat("Output tokens (time range)", f'sum(increase(phoneborg_gateway_tokens_total{{{OUT}}}[$__range])) or vector(0)', 18, w=6)
for p_ in panels[-4:]:
    p_["fieldConfig"]["defaults"]["decimals"] = 0
y[0] += 4

def minute_bars(title, targets, x, w, desc, repeat=None):
    """Bars, one per minute: tokens in that minute. targets = [(expr, legend, color)]."""
    panels.append({"type": "timeseries", "title": title, "id": nid(), "datasource": DS, "description": desc,
        "gridPos": {"h": 8, "w": w, "x": x, "y": y[0]}, "interval": "1m", "maxDataPoints": 1500,
        "targets": [{"refId": chr(65+i), "datasource": DS, "legendFormat": l, "expr": e} for i, (e, l, _) in enumerate(targets)],
        "fieldConfig": {"defaults": {"unit": "short", "decimals": 0, "min": 0,
            "custom": {"drawStyle": "bars", "barAlignment": 0, "lineWidth": 1, "fillOpacity": 80,
                       "stacking": {"mode": "none"}, "showPoints": "never"}},
            "overrides": [{"matcher": {"id": "byName", "options": l},
                           "properties": [{"id": "color", "value": {"mode": "fixed", "fixedColor": c}}]}
                          for _, l, c in targets]},
        "options": {"legend": {"displayMode": "table", "placement": "bottom", "calcs": ["sum", "max", "mean"]},
                    "tooltip": {"mode": "multi"}}})
    if repeat:
        panels[-1].update({"repeat": repeat, "repeatDirection": "h", "maxPerRow": 2})

TOK = 'phoneborg_gateway_tokens_total'
minute_bars("Cluster input tokens per minute", [(f'sum(increase({TOK}{{{IN}}}[1m]))', "input", "blue")], 0, 12,
            "Prompt tokens the whole cluster processed in each minute (incl. ones served from the prompt cache)")
minute_bars("Cluster output tokens per minute", [(f'sum(increase({TOK}{{{OUT}}}[1m]))', "output", "green")], 12, 12,
            "Tokens the whole cluster generated in each minute")
y[0] += 8

row("Node tokens per minute")
minute_bars("$node", [(f'sum(increase({TOK}{{{IN}, node_id="$node"}}[1m]))', "input", "blue"),
                      (f'sum(increase({TOK}{{{OUT}, node_id="$node"}}[1m]))', "output", "green")], 0, 12,
            "Input and output tokens of one node per minute; one panel per node (dashboard variable node)", repeat="node")
y[0] += 8

row("Inference gateway")
ts("Requests / s by node", [('sum by (node_id) (rate(phoneborg_gateway_requests_total[1m]))', "{{node_id}}")], 0, unit="reqps", stack=True)
ts("Request latency", [
    ('histogram_quantile(0.5, sum by (le) (rate(phoneborg_gateway_request_duration_seconds_bucket[2m])))', "p50"),
    ('histogram_quantile(0.95, sum by (le) (rate(phoneborg_gateway_request_duration_seconds_bucket[2m])))', "p95"),
    ('histogram_quantile(0.95, sum by (le) (rate(phoneborg_gateway_time_to_first_byte_seconds_bucket[2m])))', "TTFB p95")], 12, unit="s")
y[0] += 8
ts("Generation speed per node", [('phoneborg_node_generation_tokens_per_second', "{{node_id}} gen"),
                                 ('phoneborg_node_prompt_tokens_per_second', "{{node_id}} prompt")], 0, w=8,
   desc="tokens/s reported by llama.cpp for each node's last request")
ts("Token throughput", [('sum by (node_id, kind) (rate(phoneborg_gateway_tokens_total[1m]))', "{{node_id}} {{kind}}")], 8, w=8)
ts("In-flight requests", [('phoneborg_gateway_inflight_requests', "{{node_id}}")], 16, w=8)
y[0] += 8
ts("Responses by status", [('sum by (code) (rate(phoneborg_gateway_requests_total[1m]))', "{{code}}")], 0, w=8, unit="reqps")
ts("Failover / upstream errors", [('sum by (node_id) (rate(phoneborg_gateway_upstream_errors_total[1m]))', "{{node_id}}")], 8, w=8, unit="reqps",
   desc="Attempts against a node that failed and were retried on another node")
ts("Rejected requests", [('sum by (reason) (rate(phoneborg_gateway_rejected_total[1m]))', "{{reason}}")], 16, w=8, unit="reqps")
y[0] += 8
ts("Prompt cache hit ratio", [('sum by (node_id) (rate(phoneborg_gateway_tokens_total{kind="prompt_cached"}[5m])) / clamp_min(sum by (node_id) (rate(phoneborg_gateway_tokens_total{kind="prompt"}[5m])), 1e-9)', "{{node_id}}")],
   0, w=8, h=6, unit="percentunit", desc="Share of prompt tokens reused from llama-server's cache; session affinity keeps this high for agents like opencode")
ts("Routing decisions (session affinity)", [('sum by (result) (rate(phoneborg_gateway_affinity_decisions_total[5m]))', "{{result}}")], 8, w=8, h=6, unit="reqps",
   desc="hit: pinned node reused; miss: new session; spill: pinned node busy or gone")
ts("Requests by API key", [('sum by (principal) (rate(phoneborg_gateway_requests_total[5m]))', "{{principal}}")], 16, w=8, h=6, unit="reqps")
y[0] += 6

row("Usage per API key")
IO = 'kind=~"prompt|completion"'
def bars(title, targets, x, w, h=8, unit="short", desc=""):
    panels.append({"type": "bargauge", "title": title, "id": nid(), "datasource": DS, "description": desc,
        "gridPos": {"h": h, "w": w, "x": x, "y": y[0]},
        "targets": [{"refId": chr(65+i), "expr": e, "legendFormat": l, "datasource": DS, "instant": True}
                    for i, (e, l) in enumerate(targets)],
        "options": {"orientation": "horizontal", "displayMode": "gradient", "showUnfilled": True,
                    "reduceOptions": {"calcs": ["lastNotNull"]}},
        "fieldConfig": {"defaults": {"unit": unit, "decimals": 0, "min": 0}, "overrides": []}})
bars("Total tokens per API key (time range)",
     [(f'sum by (principal) (increase(phoneborg_gateway_tokens_total{{{IO}}}[$__range]))', "{{principal}}")], 0, 8,
     desc="Input + output tokens in the selected time range")
bars("Input vs output per API key (time range)",
     [('sum by (principal) (increase(phoneborg_gateway_tokens_total{kind="prompt"}[$__range]))', "{{principal}} input"),
      ('sum by (principal) (increase(phoneborg_gateway_tokens_total{kind="completion"}[$__range]))', "{{principal}} output")], 8, 8,
     desc="Input = prompt tokens (incl. cached), output = generated tokens")
bars("Input served from cache per API key",
     [('sum by (principal) (increase(phoneborg_gateway_tokens_total{kind="prompt_cached"}[$__range])) / clamp_min(sum by (principal) (increase(phoneborg_gateway_tokens_total{kind="prompt"}[$__range])), 1)', "{{principal}}")],
     16, 8, unit="percentunit", desc="Share of input tokens reused from the phones' prompt cache (free compute)")
panels[-1]["fieldConfig"]["defaults"].update({"decimals": 1, "max": 1})
y[0] += 8
ts("Token rate per API key (input / output)", [
    ('sum by (principal) (rate(phoneborg_gateway_tokens_total{kind="prompt"}[2m]))', "{{principal}} input"),
    ('sum by (principal) (rate(phoneborg_gateway_tokens_total{kind="completion"}[2m]))', "{{principal}} output")],
   0, w=24, h=7, desc="tokens per second")
y[0] += 7

row("Nodes")
ts("Node state", [('phoneborg_nodes', "{{state}}")], 0, w=8, stack=True)
ts("Available RAM", [('phoneborg_node_ram_available_bytes', "{{node_id}}")], 8, w=8, unit="bytes")
ts("Load (1m)", [('phoneborg_node_load1', "{{node_id}}")], 16, w=8)
y[0] += 8
ts("Seconds since last heartbeat", [('phoneborg_node_last_seen_age_seconds', "{{node_id}}")], 0, w=8, unit="s")
ts("Temperature", [('phoneborg_node_temperature_celsius', "{{node_id}}")], 8, w=8, unit="celsius",
   desc="Empty on emulated phones (no thermal zones)")
ts("Runtime ready / restarts", [('phoneborg_node_runtime_ready', "{{node_id}} ready"),
                                ('phoneborg_node_runtime_restarts', "{{node_id}} restarts")], 16, w=8)
y[0] += 8
ts("Benchmark CPU (synthetic)", [('phoneborg_node_benchmark_cpu_gflops', "{{node_id}}")], 0, w=12, h=6)
ts("Total RAM / cores", [('phoneborg_node_ram_total_bytes / 2^30', "{{node_id}} GiB"),
                         ('phoneborg_node_cpu_cores', "{{node_id}} cores")], 12, w=12, h=6)
y[0] += 6
ts("Self-test tok/s per node", [('phoneborg_node_runtime_gen_tokens_per_second', "{{node_id}} gen"),
                                ('phoneborg_node_runtime_prompt_tokens_per_second', "{{node_id}} prompt")], 0, w=12, h=6,
   desc="Measured against the node's own llama-server once it becomes ready and after each restart (ADR-010); this is the speed used for routing once any node reports it, unlike the synthetic benchmark above")
ts("Hot nodes", [('phoneborg_node_hot', "{{node_id}}")], 12, w=12, h=6,
   desc="1 = temperature at or above -thermal-limit-c: gets no new sessions unless every candidate node is hot (ADR-010; pbctl gateway set thermal_limit=<c>)")
y[0] += 6

row("Models")
stat("Models in catalog", 'count(phoneborg_model_info) or vector(0)', 0, desc="Models the controller keeps for nodes (pbctl models)")
stat("Models ready", 'count(phoneborg_model_download_progress == 1) or vector(0)', 4, desc="Catalog models downloaded, checked and servable to nodes")
stat("Nodes switching", 'count(phoneborg_node_model{state=~"downloading|loading"}) or vector(0)', 8,
     desc="Nodes downloading or loading a model; they get no requests until they serve it (ADR-011)")
stat("Nodes with model errors", 'count(phoneborg_node_model{state="error"}) or vector(0)', 12, thresholds=red)
stat("Planned nodes", 'sum(phoneborg_placement_plan_nodes) or vector(0)', 16, w=8, desc="Nodes the current placement plan gives a model (pbctl placement)")
y[0] += 4
ts("Planned vs serving nodes per model", [('phoneborg_placement_plan_nodes', "{{model_id}} planned"),
                                         ('count by (model_id) (phoneborg_node_model{state="serving"})', "{{model_id}} serving")], 0,
   desc="planned: nodes the placement plan gives the model; serving: nodes that report serving it")
ts("Node model states", [('count by (state) (phoneborg_node_model)', "{{state}}")], 12, stack=True,
   desc="serving, downloading, loading, error or idle, from node heartbeats")
y[0] += 8
ts("Controller model downloads", [('phoneborg_model_download_progress', "{{model_id}}")], 0, w=24, h=6, unit="percentunit",
   desc="Progress of catalog downloads on the controller (pbctl models add); 1 = ready")
y[0] += 6

row("Administration")
ts("Drained nodes", [('phoneborg_node_drained', "{{node_id}}")], 0, w=12, h=6,
   desc="1 = drained with pbctl drain: the node gets no new requests, in-flight ones finish")
ts("Admin actions", [('sum by (action, result) (increase(phoneborg_admin_actions_total[5m]))', "{{action}} {{result}}")], 12, w=12, h=6,
   desc="Admin API calls per 5 minutes; result=unauthorized means a wrong or missing admin token")
y[0] += 6

row("Pools and virtual models")
ts("Requests / s by target", [('sum by (target) (rate(phoneborg_gateway_target_requests_total[1m]))', "{{target}}")], 0, w=12, h=6,
   unit="reqps", stack=True, desc="What clients asked for: model (a served model id), auto, pool/<name> or node/<alias> (ADR-014)")
ts("Eligible nodes per pool", [('phoneborg_pool_members', "{{pool}}")], 12, w=12, h=6,
   desc="Pool members that can take requests now: ready, not drained, not hot, matching the pool's filters (pbctl pools)")

LINKS = [{"title": "PhoneBorg on GitHub", "type": "link", "icon": "doc",
          "url": "https://github.com/dzaczek/phoneborg", "targetBlank": True}]

def save(uid, title, filename, templating, time_from="now-30m"):
    """Writes the panels built so far as one dashboard, then starts a new one."""
    dash = {"uid": uid, "title": title, "tags": ["phoneborg"], "timezone": "browser",
            "schemaVersion": 39, "version": 1, "refresh": "5s", "time": {"from": time_from, "to": "now"},
            "links": LINKS, "templating": {"list": templating}, "panels": list(panels)}
    out = os.path.join(os.path.dirname(os.path.abspath(__file__)), "dashboards", filename)
    json.dump(dash, open(out, "w"), indent=1)
    print(filename, len(panels), "panels")
    panels.clear(); pid[0] = 1; y[0] = 0

save("phoneborg", "PhoneBorg", "phoneborg.json", [{"name": "node", "label": "Node", "type": "query", "datasource": DS,
    "query": {"query": 'label_values(phoneborg_gateway_tokens_total, node_id)', "refId": "node"},
    "definition": 'label_values(phoneborg_gateway_tokens_total, node_id)',
    "refresh": 2, "multi": True, "includeAll": True, "sort": 1,
    "current": {"selected": True, "text": ["All"], "value": ["$__all"]}}])

# ---------- Node analytics: one phone at a time (ADR-031) ----------
N = 'node_id="$node"'
REQ = 'phoneborg_gateway_requests_total'
row("Now")
hot = [{"color": "green", "value": None}, {"color": "orange", "value": 60}, {"color": "red", "value": 75}]
stat("Active", f'phoneborg_node_up{{{N}}}', 0, w=3, thresholds=[{"color": "red", "value": None}, {"color": "green", "value": 1}],
     desc="1 = ACTIVE")
stat("Temperature", f'phoneborg_node_temperature_celsius{{{N}}}', 3, w=3, unit="celsius", thresholds=hot)
stat("Battery", f'phoneborg_node_battery_level_percent{{{N}}}', 6, w=3, unit="percent")
stat("Available RAM", f'phoneborg_node_ram_available_bytes{{{N}}}', 9, w=3, unit="bytes")
stat("Self-test", f'phoneborg_node_runtime_gen_tokens_per_second{{{N}}}', 12, w=3, desc="Generation tok/s of the node's self-test (ADR-010)")
stat("Last request", f'phoneborg_node_generation_tokens_per_second{{{N}}}', 15, w=3, desc="Generation tok/s llama.cpp reported for the last request")
stat("In flight", f'phoneborg_gateway_inflight_requests{{{N}}} or vector(0)', 18, w=3)
stat("Output (range)", f'sum(increase({TOK}{{{OUT}, {N}}}[$__range])) or vector(0)', 21, w=3, desc="Tokens this phone generated in the selected time range")
panels[-1]["fieldConfig"]["defaults"]["decimals"] = 0
y[0] += 4

row("Tokens")
minute_bars("Input and output tokens per minute", [(f'sum(increase({TOK}{{{IN}, {N}}}[1m]))', "input", "blue"),
                                                   (f'sum(increase({TOK}{{{OUT}, {N}}}[1m]))', "output", "green")], 0, 12,
            "Prompt tokens the phone read and tokens it generated, per minute")
ts("Output tokens per minute by model", [(f'sum by (model) (increase({TOK}{{{OUT}, {N}}}[1m]))', "{{model}}")], 12, unit="short", stack=True)
y[0] += 8
bars("Tokens by API key (time range)", [(f'sum by (principal) (increase({TOK}{{{IO}, {N}}}[$__range]))', "{{principal}}")], 0, 8,
     desc="Who used this phone: input + output tokens per API key or principal")
ts("Average tokens per request", [
    (f'sum(increase({TOK}{{{IN}, {N}}}[5m])) / clamp_min(sum(increase({REQ}{{code=~"2..", {N}}}[5m])), 1)', "input"),
    (f'sum(increase({TOK}{{{OUT}, {N}}}[5m])) / clamp_min(sum(increase({REQ}{{code=~"2..", {N}}}[5m])), 1)', "output")], 8, w=8,
   desc="Prompt and answer size of the requests this phone served (5-minute windows)")
ts("Prompt cache hit ratio", [(f'sum(rate({TOK}{{kind="prompt_cached", {N}}}[5m])) / clamp_min(sum(rate({TOK}{{{IN}, {N}}}[5m])), 1e-9)', "cached")],
   16, w=8, unit="percentunit", desc="Share of prompt tokens reused from llama-server's cache")
y[0] += 8

row("Requests and speed")
ts("Requests per minute by status", [(f'sum by (code) (increase({REQ}{{{N}}}[1m]))', "{{code}}")], 0, w=8, stack=True)
ts("Upstream errors per minute", [(f'sum(increase(phoneborg_gateway_upstream_errors_total{{{N}}}[1m]))', "errors")], 8, w=8,
   desc="Attempts on this phone that failed and were retried elsewhere")
ts("Speed of the last request", [(f'phoneborg_node_generation_tokens_per_second{{{N}}}', "generation"),
                                 (f'phoneborg_node_prompt_tokens_per_second{{{N}}}', "prompt")], 16, w=8, desc="tokens/s reported by llama.cpp")
y[0] += 8

row("Health")
ts("Temperature", [(f'phoneborg_node_temperature_celsius{{{N}}}', "°C")], 0, w=8, unit="celsius")
ts("Available RAM", [(f'phoneborg_node_ram_available_bytes{{{N}}}', "available")], 8, w=8, unit="bytes")
ts("Load (1m)", [(f'phoneborg_node_load1{{{N}}}', "load")], 16, w=8)
y[0] += 8
ts("Battery", [(f'phoneborg_node_battery_level_percent{{{N}}}', "%")], 0, w=8, unit="percent")
ts("Runtime ready / restarts", [(f'phoneborg_node_runtime_ready{{{N}}}', "ready"), (f'phoneborg_node_runtime_restarts{{{N}}}', "restarts")], 8, w=8)
ts("Hot / drained", [(f'phoneborg_node_hot{{{N}}}', "hot"), (f'phoneborg_node_drained{{{N}}}', "drained")], 16, w=8)
y[0] += 8

row("Model and Super Borg")
ts("Model served", [(f'phoneborg_node_model{{{N}}}', "{{model_id}} {{state}}")], 0, w=12, h=6,
   desc="1 while the phone serves (or switches to) the model")
ts("Super Borg subtasks per 5 min", [(f'sum by (result) (increase(phoneborg_superborg_delegations_total{{{N}}}[5m]))', "{{result}}")], 12, w=12, h=6,
   desc="Subtasks the Super Borg orchestrator gave this phone (ADR-020)")
y[0] += 6

save("phoneborg-node", "PhoneBorg node", "phoneborg-node.json", [{"name": "node", "label": "Node", "type": "query", "datasource": DS,
    "query": {"query": 'label_values(phoneborg_node_up, node_id)', "refId": "node"},
    "definition": 'label_values(phoneborg_node_up, node_id)', "refresh": 2, "multi": False, "includeAll": False, "sort": 1}],
    time_from="now-6h")

# ---------- Token analytics (ADR-032) ----------
row("Totals in the time range")
stat("Input tokens", f'sum(increase({TOK}{{{IN}}}[$__range])) or vector(0)', 0)
stat("Output tokens", f'sum(increase({TOK}{{{OUT}}}[$__range])) or vector(0)', 4)
stat("Input from cache", f'sum(increase({TOK}{{kind="prompt_cached"}}[$__range])) or vector(0)', 8,
     desc="Prompt tokens llama-server reused from its cache instead of processing them again")
stat("Cache share", f'sum(increase({TOK}{{kind="prompt_cached"}}[$__range])) / clamp_min(sum(increase({TOK}{{{IN}}}[$__range])), 1) or vector(0)', 12,
     unit="percentunit")
stat("Output per input", f'sum(increase({TOK}{{{OUT}}}[$__range])) / clamp_min(sum(increase({TOK}{{{IN}}}[$__range])), 1) or vector(0)', 16,
     desc="Generated tokens per prompt token: agents with long prompts are far below 1")
stat("Requests", f'sum(increase({REQ}{{code=~"2.."}}[$__range])) or vector(0)', 20)
for p_ in panels[-6:]:
    p_["fieldConfig"]["defaults"]["decimals"] = 0
panels[-3]["fieldConfig"]["defaults"]["decimals"] = 1
panels[-2]["fieldConfig"]["defaults"]["decimals"] = 2
y[0] += 4

row("Who and where")
bars("By phone (time range)", [(f'sum by (node_id) (increase({TOK}{{{IN}}}[$__range]))', "{{node_id}} input"),
                               (f'sum by (node_id) (increase({TOK}{{{OUT}}}[$__range]))', "{{node_id}} output")], 0, 8)
bars("By model (time range)", [(f'sum by (model) (increase({TOK}{{{IN}}}[$__range]))', "{{model}} input"),
                               (f'sum by (model) (increase({TOK}{{{OUT}}}[$__range]))', "{{model}} output")], 8, 8)
bars("By API key (time range)", [(f'sum by (principal) (increase({TOK}{{{IN}}}[$__range]))', "{{principal}} input"),
                                 (f'sum by (principal) (increase({TOK}{{{OUT}}}[$__range]))', "{{principal}} output")], 16, 8)
y[0] += 8

row("Over time")
ts("Output tokens per minute by model", [(f'sum by (model) (increase({TOK}{{{OUT}}}[1m]))', "{{model}}")], 0, stack=True)
ts("Input tokens per minute by phone", [(f'sum by (node_id) (increase({TOK}{{{IN}}}[1m]))', "{{node_id}}")], 12, stack=True)
y[0] += 8
ts("Tokens per minute by API key", [(f'sum by (principal) (increase({TOK}{{{IO}}}[1m]))', "{{principal}}")], 0, stack=True)
ts("Prompt cache hit ratio by model", [(f'sum by (model) (rate({TOK}{{kind="prompt_cached"}}[5m])) / clamp_min(sum by (model) (rate({TOK}{{{IN}}}[5m])), 1e-9)', "{{model}}")],
   12, unit="percentunit", desc="Gemma, LFM2 and Granite-H get no cache reuse in llama.cpp (BENCHMARKS.md)")
y[0] += 8

row("Request size and speed")
ts("Average input tokens per request by model", [
    (f'sum by (model) (increase({TOK}{{{IN}}}[5m])) / clamp_min(sum by (model) (increase({REQ}{{code=~"2.."}}[5m])), 1)', "{{model}}")], 0, w=8,
   desc="How long the prompts are; every token must be read at the phone's prompt speed")
ts("Average output tokens per request by model", [
    (f'sum by (model) (increase({TOK}{{{OUT}}}[5m])) / clamp_min(sum by (model) (increase({REQ}{{code=~"2.."}}[5m])), 1)', "{{model}}")], 8, w=8)
ts("Output tokens / s by phone", [(f'sum by (node_id) (rate({TOK}{{{OUT}}}[1m]))', "{{node_id}}")], 16, w=8, stack=True)
y[0] += 8

save("phoneborg-tokens", "PhoneBorg tokens", "phoneborg-tokens.json", [], time_from="now-24h")
