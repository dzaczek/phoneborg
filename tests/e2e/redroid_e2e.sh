#!/usr/bin/env bash
# End-to-end test against emulated phones (redroid) using real adb.
#
# Verifies: provisioning over adb, registration, inventory honours the phone's
# RAM limit, benchmark -> ACTIVE, LLM serving through the gateway on both
# phones, heartbeat loss -> SUSPECT/OFFLINE with zero failed requests under
# load, recovery, and re-registration after a controller restart.
set -euo pipefail
cd "$(dirname "$0")/../.."

COMPOSE="docker compose -f deploy/docker-compose.yml"
CTRL=http://127.0.0.1:${CONTROLLER_PORT:-18080}
PHONES=(127.0.0.1:5555 127.0.0.1:5556)
MODEL=${MODEL:-models/qwen2.5-0.5b-instruct-q4_k_m.gguf}
TMPD=$(mktemp -d)

log()  { printf '\n\033[1m== %s\033[0m\n' "$*"; }
fail() { printf '\033[31mFAIL: %s\033[0m\n' "$*"; $COMPOSE logs --tail 30 controller || true; exit 1; }

# nodes_json | jq-free query helper: prints "id state ram_total" per node.
nodes() { curl -fsS "$CTRL/v1/nodes" | python3 -c '
import json,sys
for n in json.load(sys.stdin): print(n["id"], n["state"], n["inventory"]["ram_total_bytes"], n["inventory"]["cpu_cores"])'; }

wait_for() { # wait_for <timeout_s> <description> <command...>
  local t=$1 d=$2; shift 2
  for _ in $(seq "$t"); do "$@" >/dev/null 2>&1 && return 0; sleep 1; done
  fail "timeout (${t}s): $d"
}
count_state() { [ "$(nodes | awk -v s="$1" '$2==s' | wc -l | tr -d ' ')" -ge "$2" ]; }
booted() { [ "$(adb -s "$1" shell getprop sys.boot_completed 2>/dev/null | tr -d '\r')" = 1 ]; }
connect_booted() { adb connect "$1" >/dev/null 2>&1; booted "$1"; }

log "build"
make -s agent pcprov "$MODEL"
[ -x bin/llama/llama-server ] || make -s llama

log "start cluster"
sh deploy/colima-binder.sh
$COMPOSE up -d --build
wait_for 60 "controller healthy" curl -fsS "$CTRL/healthz"

log "wait for phones to boot"
for p in "${PHONES[@]}"; do wait_for 180 "$p boot" connect_booted "$p"; echo "$p booted"; done

log "provision"
args=(); for p in "${PHONES[@]}"; do args+=(-connect "$p"); done
bin/pcprov provision -controller-port "${CONTROLLER_PORT:-18080}" "${args[@]}" -agent-args "-bench-duration 1s" -model "$MODEL"

log "expect 2 ACTIVE nodes"
wait_for 60 "2 nodes ACTIVE" count_state ACTIVE 2
nodes

log "inventory reflects emulated phone limits"
nodes | awk '$3 == 2147483648 && $4 == 2 {low=1} $3 == 3221225472 && $4 == 4 {mid=1} END {exit !(low && mid)}' \
  || fail "expected one node with 2GiB/2 cores and one with 3GiB/4 cores"
echo ok

log "metrics exposed"
curl -fsS "$CTRL/metrics" | grep -E '^phoneborg_nodes\{state="ACTIVE"\} 2$' || fail "phoneborg_nodes metric"

log "gateway: model ready on both phones, load spread across them"
models_ready() { curl -fsS "$CTRL/v1/models" | grep -q '"nodes":2'; }
wait_for 120 "model ready on 2 nodes" models_ready
LOAD="python3 tests/load/chat_load.py --url $CTRL"
$LOAD -n 20 -c 2 > $TMPD/load1.txt || { cat $TMPD/load1.txt; fail "requests failed"; }
tail -4 $TMPD/load1.txt
grep '^by node:' $TMPD/load1.txt | grep -q "$(nodes | awk 'NR==1{print $1}')" && \
  grep '^by node:' $TMPD/load1.txt | grep -q "$(nodes | awk 'NR==2{print $1}')" || fail "load not spread over both nodes"

log "heartbeat loss under load: freeze phone-low, no request may fail"
LOW_ID=$(nodes | awk '$3 == 2147483648 {print $1}')
state_of() { [ "$(nodes | awk -v id="$LOW_ID" '$1==id {print $2}')" = "$1" ]; }
$LOAD -d 45 -c 2 > $TMPD/load2.txt 2>&1 & LOAD_PID=$!
sleep 3
$COMPOSE pause phone-low
wait_for 30 "phone-low SUSPECT" state_of SUSPECT; echo SUSPECT
wait_for 30 "phone-low OFFLINE" state_of OFFLINE; echo OFFLINE
wait "$LOAD_PID" || { cat $TMPD/load2.txt; fail "requests failed while phone-low was frozen"; }
tail -4 $TMPD/load2.txt
$COMPOSE unpause phone-low
wait_for 60 "phone-low ACTIVE again" state_of ACTIVE; echo ACTIVE again
wait_for 60 "model ready on 2 nodes again" models_ready

log "controller restart: agents must re-register"
$COMPOSE restart controller
wait_for 60 "controller healthy" curl -fsS "$CTRL/healthz"
wait_for 90 "2 nodes ACTIVE after restart" count_state ACTIVE 2
wait_for 60 "model served again after restart" models_ready
$LOAD -n 4 -c 2 >/dev/null || fail "requests failed after controller restart"
nodes

log "PASS"
