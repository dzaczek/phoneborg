#!/usr/bin/env bash
# Runs a small GGUF model on each phone with llama.cpp: llama-bench (tokens/s)
# and one chat completion through llama-server's OpenAI-compatible API.
#
#   tests/e2e/llm_smoke.sh [serial ...]        (default: all adb devices in "device" state)
#
# Env: MODEL (local .gguf), LLAMA_BIN (dir with llama-bench/llama-server), PROMPT.
set -euo pipefail
cd "$(dirname "$0")/../.."

MODEL=${MODEL:-models/qwen2.5-0.5b-instruct-q4_k_m.gguf}
LLAMA_BIN=${LLAMA_BIN:-bin/llama}
PROMPT=${PROMPT:-"In one sentence: why reuse old phones as AI compute nodes?"}
DIR=/data/local/tmp/phoneborg
PORT=18091  # not 18090: that one is used by the node-agent runtime

log()  { printf '\n\033[1m== %s\033[0m\n' "$*"; }
fail() { printf '\033[31mFAIL [%s]: %s\033[0m\n' "$S" "$*"; FAILED=$((FAILED+1)); }

[ -f "$MODEL" ] || { echo "model not found: $MODEL"; exit 1; }
[ -x "$LLAMA_BIN/llama-server" ] || { echo "llama.cpp not built: make llama"; exit 1; }
MODEL_NAME=$(basename "$MODEL")
MODEL_SIZE=$(stat -f%z "$MODEL" 2>/dev/null || stat -c%s "$MODEL")

SERIALS=("$@")
[ ${#SERIALS[@]} -gt 0 ] || SERIALS=($(adb devices | awk 'NR>1 && $2=="device" {print $1}'))
FAILED=0

for S in "${SERIALS[@]}"; do
  if [ "$(adb -s "$S" get-state 2>/dev/null)" != device ]; then
    log "$S"; fail "adb state is '$(adb -s "$S" get-state 2>&1)', expected 'device'"; continue
  fi
  log "$S: $(adb -s "$S" shell getprop ro.product.model | tr -d '\r')"
  dsh() { adb -s "$S" shell "$@" | tr -d '\r'; }

  # Preflight: CPU features and memory, discovered at runtime.
  if ! dsh "grep -qw asimddp /proc/cpuinfo && echo yes" | grep -q yes; then
    fail "CPU lacks dotprod (asimddp); rebuild without dotprod: make llama ARM_ARCH=armv8.2-a+fp16 (or armv8-a)"; continue
  fi
  THREADS=$(dsh "n=\$(nproc); q=\$(cat /sys/fs/cgroup/cpu.max 2>/dev/null); set -- \$q; \
    if [ \"\$1\" != max ] && [ -n \"\$2\" ]; then c=\$(( (\$1 + \$2 - 1) / \$2 )); [ \$c -lt \$n ] && n=\$c; fi; echo \$n")
  AVAIL=$(dsh "awk '/MemAvailable/ {print \$2*1024}' /proc/meminfo")
  LIMIT=$(dsh "cat /sys/fs/cgroup/memory.max 2>/dev/null; true")
  USED=$(dsh "cat /sys/fs/cgroup/memory.current 2>/dev/null; true")
  if [[ "$LIMIT" =~ ^[0-9]+$ && "$USED" =~ ^[0-9]+$ ]] && (( LIMIT - USED < AVAIL )); then AVAIL=$((LIMIT - USED)); fi
  echo "threads=$THREADS ram_avail=$((AVAIL >> 20))MiB model=$((MODEL_SIZE >> 20))MiB"
  if (( AVAIL < MODEL_SIZE * 3 / 2 )); then fail "not enough RAM for model (+50% headroom)"; continue; fi

  # Push (model only if missing or different size).
  dsh "mkdir -p $DIR/bin $DIR/models"
  adb -s "$S" push -q "$LLAMA_BIN/llama-bench" "$LLAMA_BIN/llama-server" "$DIR/bin/" >/dev/null
  dsh "chmod 755 $DIR/bin/*"
  if [ "$(dsh "stat -c%s $DIR/models/$MODEL_NAME 2>/dev/null")" != "$MODEL_SIZE" ]; then
    echo "pushing model..."; adb -s "$S" push -q "$MODEL" "$DIR/models/" >/dev/null
  fi

  log "$S: llama-bench"
  dsh "cd $DIR && bin/llama-bench -m models/$MODEL_NAME -t $THREADS -p 64 -n 32 -r 2 2>&1" \
    | grep -E '^\|' || fail "llama-bench failed"

  log "$S: llama-server chat completion"
  dsh "cd $DIR && (if [ -f server.pid ]; then kill \$(cat server.pid) 2>/dev/null; fi); \
    setsid nohup bin/llama-server -m models/$MODEL_NAME -t $THREADS -c 1024 --host 127.0.0.1 --port $PORT \
    </dev/null >server.log 2>&1 & echo \$! > server.pid"
  LOCAL=$(adb -s "$S" forward tcp:0 tcp:$PORT)
  ok=0
  for _ in $(seq 60); do curl -fs "http://127.0.0.1:$LOCAL/health" >/dev/null 2>&1 && { ok=1; break; }; sleep 1; done
  if [ $ok = 1 ]; then
    curl -fsS "http://127.0.0.1:$LOCAL/v1/chat/completions" -H 'Content-Type: application/json' \
      -d "$(python3 -c 'import json,sys; print(json.dumps({"messages":[{"role":"user","content":sys.argv[1]}],"max_tokens":64,"temperature":0}))' "$PROMPT")" \
    | python3 -c '
import json,sys
r=json.load(sys.stdin); t=r.get("timings",{})
print("answer:", r["choices"][0]["message"]["content"].strip())
print("prompt: %.1f tok/s, generation: %.1f tok/s (%d tokens)" % (t.get("prompt_per_second",0), t.get("predicted_per_second",0), t.get("predicted_n",0)))' \
      || fail "chat completion failed"
  else
    fail "llama-server did not become healthy"; dsh "tail -n 20 $DIR/server.log"
  fi
  dsh "cd $DIR && kill \$(cat server.pid) 2>/dev/null; rm -f server.pid"
  adb -s "$S" forward --remove tcp:$LOCAL
done

[ $FAILED -eq 0 ] && log PASS || { log "FAILED on $FAILED device(s)"; exit 1; }
