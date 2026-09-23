#!/usr/bin/env python3
"""Generates deploy/grafana/dashboards/phoneborg.json. Edit this file, not the JSON.

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

dash = {"uid": "phoneborg", "title": "PhoneBorg", "tags": ["phoneborg"], "timezone": "browser",
        "schemaVersion": 39, "version": 1, "refresh": "5s", "time": {"from": "now-30m", "to": "now"},
        "panels": panels}
out = os.path.join(os.path.dirname(os.path.abspath(__file__)), "dashboards", "phoneborg.json")
json.dump(dash, open(out, "w"), indent=1)
print(len(panels), "panels")
