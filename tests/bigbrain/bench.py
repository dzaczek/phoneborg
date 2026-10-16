#!/usr/bin/env python3
"""Big Brain benchmark (ADR-034): bench.py <pool> <model> <node,node,...> [ctx]

Creates or updates the Big Brain pool through the admin API, waits until its
head is ready, measures it (straight to the head, and once through the
gateway as pool/<name>) and appends one JSON line to $BENCH_OUT. Run it on
the controller's host. Environment: PHONEBORG_URL (http://127.0.0.1:18080),
PHONEBORG_ADMIN_TOKEN_FILE (/srv/phoneborg/admin-token), BENCH_OUT
(bench.jsonl)."""
import json, os, sys, time, urllib.request

API = os.environ.get("PHONEBORG_URL", "http://127.0.0.1:18080")
TOKEN = open(os.environ.get("PHONEBORG_ADMIN_TOKEN_FILE", "/srv/phoneborg/admin-token")).read().strip()
OUT = os.environ.get("BENCH_OUT", "bench.jsonl")

def call(method, path, body=None, auth=True, timeout=60, base=API):
    req = urllib.request.Request(base + path, method=method,
                                 data=None if body is None else json.dumps(body).encode())
    req.add_header("Content-Type", "application/json")
    if auth:
        req.add_header("Authorization", "Bearer " + TOKEN)
    with urllib.request.urlopen(req, timeout=timeout) as r:
        return json.loads(r.read() or b"null"), dict(r.headers)

def pool(name):
    for p in call("GET", "/admin/pools")[0]["pools"]:
        if p["name"] == name:
            return p

def nodes():
    return {n["id"]: n for n in call("GET", "/admin/nodes")[0]}

SHORT = "Wyjaśnij po polsku w 5 zdaniach, jak działa pamięć podręczna procesora."
LONG = ("Below is a technical note. Summarise it in two sentences.\n\n" +
        " ".join(["A phone cluster serves language models from old Android devices connected over USB to one host. "
                  "Each phone runs a small inference server, and a controller routes requests, tracks heat, "
                  "memory and speed, and moves models between phones when the plan changes."] * 9))

def complete(port, prompt, max_tokens, base=None, model=None):
    body = {"messages": [{"role": "user", "content": prompt}], "max_tokens": max_tokens,
            "temperature": 0.7, "cache_prompt": False, "chat_template_kwargs": {"enable_thinking": False}}
    if model:
        body["model"] = model
    t0 = time.time()
    res, hdr = call("POST", "/v1/chat/completions", body, auth=False, timeout=1800,
                    base=base or f"http://127.0.0.1:{port}")
    t = res.get("timings", {})
    return {"prompt_n": t.get("prompt_n"), "pp_tps": round(t.get("prompt_per_second", 0), 2),
            "gen_n": t.get("predicted_n"), "gen_tps": round(t.get("predicted_per_second", 0), 3),
            "wall_s": round(time.time() - t0, 1), "node": hdr.get("X-Phoneborg-Node") or hdr.get("X-PhoneBorg-Node")}

def main():
    name, model, members = sys.argv[1], sys.argv[2], sys.argv[3].split(",")
    ctx = int(sys.argv[4]) if len(sys.argv) > 4 else 4096
    t0 = time.time()
    before = pool(name)
    same = (before and before.get("model") == model and before.get("nodes") == members and not before.get("disabled")
            and (before.get("ctx_size") or 4096) == ctx)
    started_before = ((before or {}).get("big_brain") or {}).get("load_started_at")
    call("PUT", "/admin/pools/" + name, {"routing": "bigbrain", "model": model, "nodes": members, "ctx_size": ctx,
                                          "description": "benchmark"})
    seen, error_since = None, None
    while True:
        p = pool(name)
        bb = p.get("big_brain") or {}
        state = (bb.get("state"), bb.get("reason", "")[:120])
        if state != seen:
            print(f"[{time.time()-t0:6.0f}s] {state[0]}: {state[1]}", flush=True)
            seen = state
        # A changed pool must show a head started after the change.
        fresh = same or (bb.get("load_started_at") not in (None, started_before) and len(bb.get("tensor_split") or []) == len(members))
        if bb.get("state") == "ready" and fresh:
            break
        error_since = (error_since or time.time()) if bb.get("state") == "error" else None
        if (error_since and time.time() - error_since > 90) or time.time() - t0 > 2400:
            rec = {"pool": name, "model": model, "nodes": members, "error": bb.get("reason"), "at": time.strftime("%FT%TZ", time.gmtime())}
            open(OUT, "a").write(json.dumps(rec) + "\n")
            print(json.dumps(rec)); return 1
        time.sleep(5)
    port = bb["port"]
    ns = nodes()
    free = {m: round((ns.get(m) or next((n for n in ns.values() if n.get("alias") == m), {})).get("last_heartbeat", {}).get("ram_avail_bytes", 0) / 1e9, 2) for m in members}
    runs = [complete(port, SHORT, 128), complete(port, SHORT, 128), complete(port, LONG, 32)]
    gw = complete(None, SHORT, 32, base=API, model="pool/" + name)
    p = pool(name)
    bb = p["big_brain"]
    rec = {"pool": name, "model": model, "nodes": members, "ctx": ctx, "ready_after_s": round(time.time() - t0),
           "load_s": round(bb.get("load_seconds", 0)), "tensor_split_mib": bb.get("tensor_split"),
           "need_gb": round(bb["need_bytes"] / 1e9, 2), "have_gb": round(bb["have_bytes"] / 1e9, 2),
           "free_after_load_gb": free, "selftest_gen_tps": bb.get("gen_tps"),
           "short": runs[:2], "long": runs[2], "gateway": gw, "at": time.strftime("%FT%TZ", time.gmtime())}
    open(OUT, "a").write(json.dumps(rec) + "\n")
    print(json.dumps(rec, indent=1))

sys.exit(main())
