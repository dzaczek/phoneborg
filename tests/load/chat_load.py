#!/usr/bin/env python3
"""Load generator for the PhoneBorg gateway (OpenAI-compatible API).

    tests/load/chat_load.py -n 40 -c 3                # 40 requests, 3 concurrent
    tests/load/chat_load.py -d 300 -c 2 --stream 0.5  # 5 minutes, half streaming

Prints which node served each request (X-PhoneBorg-Node) and a summary.
Stdlib only.
"""
import argparse
import collections
import json
import random
import statistics
import threading
import time
import urllib.error
import urllib.request

PROMPTS = [
    "In one sentence: why reuse old phones as AI compute nodes?",
    "Name three uses of a small language model on a phone.",
    "Explain what a heartbeat is in a distributed system, briefly.",
    "Write a haiku about a cluster of old smartphones.",
    "What is quantization in machine learning? One paragraph.",
    "List two risks of running servers on Android phones.",
    "Translate to Polish: the cluster is healthy.",
    "Give a one-line tip for reducing phone battery wear.",
]


def one(args, lock, stats, i):
    body = {
        "model": args.model,
        "messages": [{"role": "user", "content": random.choice(PROMPTS)}],
        "max_tokens": args.max_tokens,
        "stream": random.random() < args.stream,
    }
    req = urllib.request.Request(args.url + "/v1/chat/completions", data=json.dumps(body).encode(),
                                 headers={"Content-Type": "application/json"})
    if args.key:
        req.add_header("Authorization", "Bearer " + args.key)
    t0 = time.time()
    node, code = "-", 0
    try:
        with urllib.request.urlopen(req, timeout=180) as r:
            node, code = r.headers.get("X-PhoneBorg-Node", "-"), r.status
            r.read()
    except urllib.error.HTTPError as e:
        code = e.code
    except Exception as e:  # connection errors
        code = type(e).__name__
    dt = time.time() - t0
    with lock:
        stats["by_node"][node] += 1
        stats["codes"][code] += 1
        stats["lat"].append(dt)
        print(f"#{i:<4} {str(code):<5} node={node:<18} stream={str(body['stream']):<5} {dt:6.2f}s", flush=True)


def main():
    p = argparse.ArgumentParser()
    p.add_argument("--url", default="http://127.0.0.1:18080")
    p.add_argument("--key", default="", help="API key (if the controller uses -api-keys-file)")
    p.add_argument("--model", default="", help="empty = any model")
    p.add_argument("-n", type=int, default=20, help="number of requests (ignored with -d)")
    p.add_argument("-d", type=float, default=0, help="run for this many seconds instead of -n")
    p.add_argument("-c", type=int, default=2, help="concurrency")
    p.add_argument("--stream", type=float, default=0.3, help="fraction of streaming requests")
    p.add_argument("--max-tokens", type=int, default=48)
    args = p.parse_args()

    lock = threading.Lock()
    stats = {"by_node": collections.Counter(), "codes": collections.Counter(), "lat": []}
    counter = iter(range(1, 10**9))
    deadline = time.time() + args.d if args.d else None

    def worker():
        while True:
            with lock:
                i = next(counter)
            if (deadline and time.time() > deadline) or (not deadline and i > args.n):
                return
            one(args, lock, stats, i)

    threads = [threading.Thread(target=worker) for _ in range(args.c)]
    t0 = time.time()
    for t in threads:
        t.start()
    for t in threads:
        t.join()

    lat = sorted(stats["lat"])
    print(f"\n{len(lat)} requests in {time.time() - t0:.1f}s")
    print("by node:", dict(stats["by_node"]))
    print("status: ", dict(stats["codes"]))
    if lat:
        print(f"latency p50={statistics.median(lat):.2f}s p95={lat[int(len(lat) * 0.95) - 1 if len(lat) > 1 else 0]:.2f}s")
    ok = stats["codes"].get(200, 0)
    raise SystemExit(0 if ok == len(lat) else 1)


if __name__ == "__main__":
    main()
