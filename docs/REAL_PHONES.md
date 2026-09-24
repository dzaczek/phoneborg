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

Run these before provisioning. `pcprov` checks the ABI itself and picks the
fastest llama.cpp build the phone's CPU supports automatically (see
ADR-007, requires `make llama-all`); `tests/e2e/llm_smoke.sh` checks RAM. The
commands below let you judge a phone before you use it.

```sh
S=<SERIAL>

# Identity and Android version
adb -s $S shell getprop ro.product.manufacturer
adb -s $S shell getprop ro.product.model
adb -s $S shell getprop ro.build.version.release    # Android version
adb -s $S shell getprop ro.soc.model                # SoC (Android 12+; else ro.board.platform)

# CPU: must list arm64-v8a
adb -s $S shell getprop ro.product.cpu.abilist
# dotprod/fp16 pick which llama.cpp build pcprov uses (ADR-007); i8mm makes prompts much faster
adb -s $S shell 'grep -m1 -o -w asimddp /proc/cpuinfo || echo "no dotprod: pcprov falls back to a slower build (needs make llama-all)"'
adb -s $S shell 'grep -m1 -i features /proc/cpuinfo'   # full flag list (fphp/asimdhp = FP16)
adb -s $S shell 'grep -m1 -o -w i8mm /proc/cpuinfo || echo "no i8mm (ok)"'
# Core clusters (big.LITTLE): count of cores per max frequency in kHz
adb -s $S shell 'cat /sys/devices/system/cpu/cpu*/cpufreq/cpuinfo_max_freq | sort | uniq -c'

# Memory and storage
adb -s $S shell 'grep -E "MemTotal|MemAvailable" /proc/meminfo'
adb -s $S shell 'df -h /data | tail -1'

# Battery and temperature (temperature is in tenths of °C)
adb -s $S shell 'dumpsys battery | grep -E "powered|level|temperature|health"'
```

Gemma 3n is built for phones. Some of its weights (per-layer embeddings) are
read from the memory-mapped file only when needed, so llama.cpp keeps less
than the file size resident. The controller's catalog detects this
(`sparse_bytes`/`resident_bytes`, ADR-012 addendum) and sizes and predicts
speed from resident bytes, not the file size, so this no longer causes the
agent to refuse the model or the planner to warn it does not fit.

Rules of thumb:
- **RAM:** `MemAvailable` should be at least 1.5× the model file.
- **Thermals:** a battery temperature above ~450 (45 °C) under load means the
  phone needs cooling.

## 4. Keep the phone awake while charging

A phone whose CPU sleeps stops sending heartbeats and drops to `SUSPECT`.

Measured on a Xiaomi Mi 8 with LineageOS 22.2: with the screen off and
stay-awake disabled, the node stayed `ACTIVE` for 40 minutes on USB power, with
40/40 requests served at ~17 tok/s and heartbeats never older than 5 s. So on
LineageOS this step is optional. Vendor ROMs (MIUI, One UI) can behave
differently, so run the same check before relying on it: turn the screen off
and watch `pbctl nodes` for a while.

```sh
adb -s $S shell svc power stayon usb                       # stay awake while on USB
adb -s $S shell settings get global stay_on_while_plugged_in   # 2 = USB, 0 = off
adb -s $S shell settings put system screen_brightness 0    # reduce heat and burn-in
```

To undo: `adb -s $S shell settings put global stay_on_while_plugged_in 0`.

**Battery.** A phone on USB 24/7 sits at 100%, which wears the battery and can
make it swell. If the ROM offers a charge limit, enable it: LineageOS
Settings → Battery → Charging control (not every device supports it), or
Samsung "Protect battery". Check phones for a bulging back cover regularly.

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

## Free RAM on a dedicated phone (slim)

A phone that only serves the cluster does not need its camera, gallery,
browser or other apps sitting in memory. `pcprov slim` disables a
conservative, vendor-extensible allowlist of user-facing apps to free that
RAM for `llama-server`. `pcprov unslim` reverses it exactly.

```sh
bin/pcprov slim   -serial $S                        # disable non-essential apps
bin/pcprov slim   -serial $S -slim-telephony        # also disable dialer/contacts (off by default)
bin/pcprov slim   -all                              # every ready device
bin/pcprov unslim -serial $S                        # restore everything slim disabled

# Or slim right after provisioning:
bin/pcprov provision -serial $S -model models/qwen2.5-0.5b-instruct-q4_k_m.gguf -slim
bin/pcprov watch      -serial $S -model models/qwen2.5-0.5b-instruct-q4_k_m.gguf -slim
```

What `slim` disables, if installed (`pm disable-user --user 0`): camera
(org.lineageos.aperture, com.android.camera2, org.codeaurora.snapcam),
gallery (org.lineageos.glimpse, com.android.gallery3d), music
(org.lineageos.twelve, com.android.music), browser (org.lineageos.jelly,
com.android.browser), calendar (org.lineageos.etar, com.android.calendar),
clock (com.android.deskclock), recorder (org.lineageos.recorder), messaging
(com.android.messaging), email (com.android.email); with `-slim-telephony`,
also dialer/contacts (com.android.dialer, com.android.contacts). It also
finishes a pending setup wizard (`settings put secure user_setup_complete 1`,
`settings put global device_provisioned 1`, then `am force-stop` on the setup
wizard package — never disabled) and `am force-stop`s `com.android.settings`
(it restarts on demand, so this only frees memory, it does not remove it).

`slim` never disables system UI, the launcher, Settings, telephony services,
input methods, or anything with "provider", "permissioncontroller",
"packageinstaller", "shell", "networkstack", "bluetooth", "wifi", "nfc" or
"setupwizard" in its package name, even if the allowlist above is extended
for a new vendor.

It records exactly what it disabled in
`/data/local/tmp/phoneborg/slim.state` on the device (one package per line)
and logs `MemAvailable` (from `/proc/meminfo`) before and after, and the
freed MiB. Running it again is a no-op beyond newly-installed apps: it never
re-disables what is already off, and it merges into the same state file so
`unslim` still restores everything.

**Caution:** a slimmed phone becomes cluster-only — no camera, gallery,
browser, music, calendar, clock, recorder, messaging or email app until
`unslim` is run. Do not slim a phone still used for anything else.

## Model switching (ADR-012)

When the controller assigns a node a model (`DesiredRuntime`, from the
placement planner), the agent downloads and serves it without needing a
replug or a new `pcprov provision`:

- Files are cached in `models/*.gguf` under
  `/data/local/tmp/phoneborg` (`models/<model_id>.gguf`), the same directory
  `pcprov -model` already pushes into. A download in progress is
  `models/<model_id>.gguf.part`; it resumes with an HTTP Range request if the
  agent restarts mid-download, and a file already on disk with the right
  SHA-256 is reused without downloading it again.
- Before downloading, the agent checks free storage with `statfs`. If short,
  it deletes other cached `.gguf` files, least-recently-used first, but never
  the model currently being served. If that is still not enough, the switch
  fails with a clear `error` instead of downloading a partial model.
- Context size, slot count and KV cache type (f16 or q8_0) are chosen
  automatically to fit available RAM: `-mem-reserve-mb` (default 600) is how
  much RAM the agent leaves for Android and itself; lower it on a phone that
  is otherwise idle, raise it if the node gets killed under memory pressure.
- If a switch fails (bad download, out of RAM, llama-server crash-loops on
  the new model) and the previous model file is still on disk, the agent
  falls back to it, so the node keeps serving. `pbctl nodes` / `/v1/nodes`
  shows the failure in `runtime.state`/`runtime.error` either way.
- The static `-model` flag (pcprov) still works exactly as before: a node
  serves it until the controller sends a `DesiredRuntime`, which then takes
  over.

```sh
adb -s $S shell ls -la /data/local/tmp/phoneborg/models        # cached models
curl -s http://127.0.0.1:18080/v1/nodes | python3 -m json.tool | grep -A8 '"runtime"'
```

## Troubleshooting

| Symptom | Likely cause | What to do |
|---|---|---|
| `pcprov` fails with "unsupported ABI" | 32-bit phone | not supported |
| `pcprov` fails with "no llama.cpp build matches this phone's CPU" | SoC lacks dotprod/fp16, and the fallback build was never made | `make llama-all` (builds every variant), then re-provision |
| `agent.log` shows `signal: illegal instruction` | llama.cpp build too new for the CPU (only possible with an explicit `-llama-server` override) | drop the override and let pcprov choose the build |
| node goes `SUSPECT` when the screen turns off | CPU sleeps | step 4 |
| `phoneborg_node_runtime_restarts` keeps growing | llama-server is being killed (out of memory or vendor task killer) | smaller model or context; close apps; check `runtime.log` |
| tokens/s far below expectations | too many threads, or threads pinned to offline cores | keep the default (`-threads-policy all`); try `-agent-args "-threads 4"` and compare; see ADR-009 |
| tokens/s drops after a few minutes | thermal throttling | cooling, no case, lower brightness; watch temperature in Grafana |
| node `ACTIVE` but requests to it fail with "connection refused", or the agent logs "connection refused" to 127.0.0.1:18080 | the phone re-enumerated on USB (loose cable, hot phone) and adb dropped its forward/reverse rules | `bin/pcprov heal -serial $S`; keep `pcprov watch` running: it re-checks the links every 15 s |
| node `OFFLINE` after unplugging | expected: phones talk to the controller over USB | replug; `pcprov watch` re-provisions |
| agent missing after a phone reboot | processes started over adb do not survive a reboot | `pcprov watch` re-provisions when the phone is plugged in |
| model state stays `loading` | model still loading, or not enough RAM | `tail runtime.log`; compare `MemAvailable` with the model size |

## Wireless adb

Not supported yet. The phone must stay on the USB cable, because the
controller reaches it through `adb reverse` and `adb forward`, and the cable
also powers it. Wi-Fi nodes are on the roadmap.

## Choosing a model for a phone

Measured on a Xiaomi Mi 8 (Snapdragon 845, 5.5 GiB RAM, LineageOS 22.2,
`armv8.2-a+fp16` build, 6 threads). llama-server ran with an 8k context, a
~300-token prompt and 64 generated tokens. With no model loaded, 3.1 GiB was
available.

| Model (Q4_K_M) | File | llama-server RSS | Prompt tok/s | Generation tok/s | Verdict |
|---|---|---|---|---|---|
| Qwen2.5-0.5B-Instruct | 468 MiB | 632 MiB | 36.2 | 14.8 | fast, weak answers |
| Gemma 3 1B it | 768 MiB | 926 MiB | 16.7 | 7.3 | good |
| Llama 3.2 1B Instruct | 770 MiB | 1120 MiB | 15.8 | 8.4 | good |
| Qwen2.5-1.5B-Instruct | 1065 MiB | 1375 MiB | 11.4 | 6.8 | **best balance on this phone** |
| DeepSeek-R1-Distill-Qwen-1.5B | 1065 MiB | 1373 MiB | 9.3 | 6.0 | reasons step by step, so answers take long |
| Qwen3-1.7B | 1056 MiB | 2029 MiB | 8.8 | 4.5 | large KV cache per token |
| Gemma 3 4B it | 2374 MiB | 2839 MiB | 3.8 | 2.4 | fits, too slow for chat |
| Qwen3-4B | 2381 MiB | 3248 MiB | 3.3 | 0.3 | memory pressure; unusable |
| SmolLM3-3B | 1826 MiB | 2541 MiB | 4.3 | 2.9 | slow |
| Llama 3.2 3B Instruct | 1925 MiB | 2911 MiB | 4.2 | 2.6 | slow |
| Phi-4-mini (3.8B) | 2376 MiB | 3445 MiB | 3.4 | 0.6 | memory pressure |
| **Gemma 3n E2B it** | 2886 MiB | **1774 MiB** | 5.8 | 3.7 | **uses less RAM than its file; best 2–4B option** |
| Gemma 3n E4B it | 4328 MiB | 3032 MiB | 2.8 | 1.9 | fits a 5.5 GiB phone, slow |

Rules of thumb:
- Generation speed on phones is bound by memory bandwidth. Tokens/s drop
  roughly in proportion to model size.
- Leave at least ~1 GiB of the phone's free memory unused. Qwen3-4B fit on
  paper, but Android started evicting its pages and speed collapsed
  (0.3 tok/s).
- KV cache size differs a lot between architectures. Qwen3-1.7B needs ~4× more
  memory per context token than Qwen2.5-1.5B, so it needs a smaller context or
  q8_0 KV (the agent chooses this automatically, see ADR-012).
- `llama-server` maps weights from the file. `MemAvailable` barely drops when a
  model loads, so judge fit by RSS or by the agent's budget, not by
  `MemAvailable`.

The first rule of thumb above is now automated (ADR-015): the controller
computes each phone's memory bandwidth from `gen_tok_s * model_resident_bytes`
of its own self-test, sorts it into a performance tier (`t1`..`t4`, `pbctl
nodes`/`pbctl classes`), and predicts a candidate model's speed on it before
ever placing it there. A phone that fits a model on paper but would only run
it at, say, 2.4 tok/s like the Gemma 3 4B row above is excluded from
placement by `-min-predicted-tok-s` (default 3) unless a policy pins it
there explicitly. See ADR-015 and `docs/USAGE.md`'s "Performance tiers and
predicted speed".

"Resident bytes" (ADR-012's addendum) is the file size minus tensors
llama.cpp only reads a few rows of, such as Gemma 3n's per-layer embeddings
(1440 MiB of the 2886 MiB E2B file above): sizing, fit checks and predicted
speed all use it instead of the raw file size, which is why the Gemma 3n E2B
row above fits and predicts correctly despite its file being nearly as big as
Gemma 3 4B's.
