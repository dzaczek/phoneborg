# Adding real Android phones

This guide takes a phone from the drawer to an `ACTIVE` node serving a model.
Every command below was checked against Android 12. `<SERIAL>` is the first
column of `adb devices`.

## 1. Prepare the phone (once per phone)

1. **Enable developer options:** Settings → About phone → tap **Build number**
   7 times.
2. **Enable USB debugging:** Settings → System → Developer options → **USB
   debugging**. On Xiaomi/Redmi/Poco also enable **USB debugging (Security
   settings)**. This needs a SIM card and a Mi account.
3. **Close apps and free storage.** The model needs its file size plus about
   100 MB in `/data`. Remove the case: the phone will run hot.
4. **Use a data-capable cable** and, for more than one phone, a powered USB hub
   (2–3 A per port).

The screen is not needed after this, so phones with a cracked screen are fine
as long as touch works once.

## 2. Connect and authorize

```sh
adb devices -l
```

| State | Meaning | Fix |
|---|---|---|
| `device` | ready | – |
| `unauthorized` | the phone has not accepted this computer | unlock the phone, tap **Allow**, tick **Always allow from this computer** |
| `offline` | adb connection is stale | `adb kill-server && adb start-server`, replug the cable |
| *(not listed)* | charge-only cable, or USB debugging off | try another cable or port; check step 1.2 |

Revoked a key by mistake? Developer options → **Revoke USB debugging
authorizations**, then replug.

## 3. Preflight checks

Run these before provisioning. `pcprov` checks the ABI itself, and
`tests/e2e/llm_smoke.sh` checks dotprod and RAM. The commands below let you
judge a phone before you use it.

```sh
S=<SERIAL>

# Identity and Android version
adb -s $S shell getprop ro.product.manufacturer
adb -s $S shell getprop ro.product.model
adb -s $S shell getprop ro.build.version.release    # Android version
adb -s $S shell getprop ro.soc.model                # SoC (Android 12+; else ro.board.platform)

# CPU: must list arm64-v8a
adb -s $S shell getprop ro.product.cpu.abilist
# dotprod is required by the default llama.cpp build; i8mm makes prompts much faster
adb -s $S shell 'grep -m1 -o -w asimddp /proc/cpuinfo || echo "NO dotprod: see Troubleshooting (SIGILL)"'
adb -s $S shell 'grep -m1 -i features /proc/cpuinfo'   # fphp/asimdhp = FP16, picks the fallback build
adb -s $S shell 'grep -m1 -o -w i8mm /proc/cpuinfo || echo "no i8mm (ok)"'
# Core clusters (big.LITTLE): count of cores per max frequency in kHz
adb -s $S shell 'cat /sys/devices/system/cpu/cpu*/cpufreq/cpuinfo_max_freq | sort | uniq -c'

# Memory and storage
adb -s $S shell 'grep -E "MemTotal|MemAvailable" /proc/meminfo'
adb -s $S shell 'df -h /data | tail -1'

# Battery and temperature (temperature is in tenths of °C)
adb -s $S shell 'dumpsys battery | grep -E "powered|level|temperature|health"'
```

Rules of thumb:
- **RAM:** `MemAvailable` should be at least 1.5× the model file.
- **Thermals:** a battery temperature above ~450 (45 °C) under load means the
  phone needs cooling.

## 4. Keep the phone awake while charging

A phone whose CPU sleeps stops sending heartbeats and drops to `SUSPECT`.

```sh
adb -s $S shell svc power stayon usb                       # stay awake while on USB
adb -s $S shell settings get global stay_on_while_plugged_in   # 2 = USB, 0 = off
adb -s $S shell settings put system screen_brightness 0    # reduce heat and burn-in
```

To undo: `adb -s $S shell settings put global stay_on_while_plugged_in 0`.

## 5. Provision

Start the controller first: `make cluster-up` (Docker), or `bin/controller`
(native).

```sh
bin/pcprov devices                            # what pcprov sees

# Watch mode: provisions phones as they are plugged in, re-provisions after replug
bin/pcprov watch -serial $S \
  -model models/qwen2.5-0.5b-instruct-q4_k_m.gguf -agent-args "-ctx-size 16384"

# Or one-shot, for one phone or all ready devices
bin/pcprov provision -serial $S -model models/qwen2.5-0.5b-instruct-q4_k_m.gguf
bin/pcprov provision -all       -model models/qwen2.5-0.5b-instruct-q4_k_m.gguf
```

Notes:
- Without `-serial`, `watch` takes every device, emulated ones included.
- `-agent-args "-ctx-size 16384"` is only needed for agent clients such as
  opencode (see `DEV_EMULATION.md`).
- The first push of a 0.5 GB model takes ~30 s over USB 2.0. Later runs skip
  it when the file size matches.

## 6. Verify

```sh
# Agent process and its log
bin/pcprov status -serial $S

# The node in the controller (state, RAM, benchmark, model readiness)
curl -s http://127.0.0.1:18080/v1/nodes | python3 -m json.tool | less
curl -s http://127.0.0.1:18080/v1/models

# Port plumbing: reverse = phone -> controller, forward = controller -> llama-server
adb -s $S reverse --list        # expect: tcp:18080 tcp:18080
adb forward --list              # expect: <SERIAL> tcp:<host port> tcp:18090

# llama-server on the phone, through its forward
P=$(adb forward --list | awk -v s=$S '$1==s && $3=="tcp:18090" {sub("tcp:","",$2); print $2}')
curl -s http://127.0.0.1:$P/health               # {"status":"ok"} once the model is loaded

# Real tokens/s on this phone (uses port 18091, does not disturb the agent)
bash tests/e2e/llm_smoke.sh $S

# One request through the cluster gateway; the header shows which phone answered
curl -si http://127.0.0.1:18080/v1/chat/completions \
  -d '{"messages":[{"role":"user","content":"Hello!"}],"max_tokens":16}' | grep -iE '^x-phoneborg-node|content'
```

Dashboards: http://127.0.0.1:18080 (node table) and http://127.0.0.1:3000
(Grafana: temperature, battery, tokens/s, restarts).

## 7. Files and logs on the phone

Everything lives in `/data/local/tmp/phoneborg`:

```text
node-agent           agent binary
agent.pid, agent.log agent process and JSON log
bin/llama-server     inference server
models/*.gguf        models
runtime.pid, runtime.log   llama-server process and log
```

```sh
adb -s $S shell ls -la /data/local/tmp/phoneborg /data/local/tmp/phoneborg/models
adb -s $S shell tail -n 50 /data/local/tmp/phoneborg/agent.log
adb -s $S shell tail -n 50 /data/local/tmp/phoneborg/runtime.log
adb -s $S shell 'grep "prompt eval time" /data/local/tmp/phoneborg/runtime.log | tail -3'   # per-request speed
adb -s $S shell top -b -n 1 | head -15                                          # CPU/RAM of node-agent and llama-server
```

## 8. Stop and remove

```sh
bin/pcprov stop -serial $S                          # stops agent and llama-server
adb -s $S reverse --remove-all
adb forward --list | awk -v s=$S '$1==s {print $2}' | xargs -I{} adb -s $S forward --remove {}
adb -s $S shell rm -rf /data/local/tmp/phoneborg    # removes binaries and models
adb -s $S shell settings put global stay_on_while_plugged_in 0
```

## Troubleshooting

| Symptom | Likely cause | What to do |
|---|---|---|
| `pcprov` fails with "unsupported ABI" | 32-bit phone | not supported |
| smoke test fails with "CPU lacks dotprod", or `agent.log` shows `signal: illegal instruction` | SoC without dotprod (Snapdragon 845 and older) | build without dotprod: `make llama ARM_ARCH=armv8.2-a+fp16` if `Features` lists `fphp asimdhp`, else `ARM_ARCH=armv8-a`; then re-provision |
| node goes `SUSPECT` when the screen turns off | CPU sleeps | step 4 |
| `phoneborg_node_runtime_restarts` keeps growing | llama-server is being killed (out of memory or vendor task killer) | smaller model or context; close apps; check `runtime.log` |
| tokens/s drops after a few minutes | thermal throttling | cooling, no case, lower brightness; watch temperature in Grafana |
| node `OFFLINE` after unplugging | expected: phones talk to the controller over USB | replug; `pcprov watch` re-provisions |
| agent missing after a phone reboot | processes started over adb do not survive a reboot | `pcprov watch` re-provisions when the phone is plugged in |
| model state stays `loading` | model still loading, or not enough RAM | `tail runtime.log`; compare `MemAvailable` with the model size |

## Wireless adb

Not supported yet. The phone must stay on the USB cable, because the
controller reaches it through `adb reverse` and `adb forward`, and the cable
also powers it. Wi-Fi nodes are on the roadmap.
