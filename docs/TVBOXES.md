# Android TV boxes as PhoneBorg nodes

Started 2026-10-20 as a working note; since 2026-11-21 TV boxes are part of
the collective. The first box, an NVIDIA Shield TV (2017), joined the
reference cluster over network adb: see [Measured: NVIDIA Shield TV
(2017)](#measured-nvidia-shield-tv-2017). Figures for other boxes are still
estimates.

## Why

TV boxes are cheap, always on mains power and do not sleep like phones.
Anthropic's red team ran a 26B model split across 15 off-brand Android TV
boxes (~$55 each) with the same layer-split idea as Big Brain
([study](https://www.anthropic.com/research/self-replicating-ai)). The same
study is also the warning: cheap, poorly secured devices with network adb are
exactly the attack surface it describes.

## Measured: NVIDIA Shield TV (2017)

`darcy`, Android 11, Tegra X1 (4× Cortex-A57, no dotprod), 3 GB, Ethernet.
Its 64-bit userland and real serial needed none of the fallbacks below.

| Step | Result |
|---|---|
| Discovery | `-scan 192.168.44.0/22` (1022 hosts, routed from the controller's 10.10.100.0/24) found it in the first round; "Allow network debugging" accepted with the remote |
| Provisioning | about 30 s from pcprov start to `ACTIVE`, including the 491 MB model push over the network; build `armv8-a`, 4 threads |
| Slim | 25 packages disabled (19 by `pcprov slim`, 6 more by hand: Chromecast, Play Store, home-screen recommendations, NVIDIA telemetry and updater, smartconnect) and Android's cached-process limit lowered to 4 (`device_config put activity_manager max_cached_processes 4`). MemAvailable 0.9 → 1.6 GB, agent budget 0.51 → 1.1 GiB |
| Budget | the agent keeps a 600 MiB reserve (`-mem-reserve-mb`), which on 3 GB is most of the headroom: before the extra slimming only a 350M model fitted |

`pbctl mmb run nodes=shield` after slimming (generation speed, short answer
latency):

| Model | Gen tok/s | Prompt tok/s | Short answer |
|---|---|---|---|
| LFM2-350M Q4_K_M | **17.9** | 39.9 | 0.6 s |
| Qwen2.5-0.5B Q4_K_M | 10.3 | 21.4 | 1.1 s |
| LFM2-700M Q4_K_M | 9.8 | 18.9 | 1.2 s |
| Qwen3-0.6B Q4_K_M | 9.0 | 22.5 | 1.1 s |
| Gemma 3 1B Q4_K_L | 6.0 | 11.3 | 2.2 s |
| Llama 3.2 1B Q4_K_M | 5.4 | 11.5 | 1.7 s |

The reference cluster runs the scan from a systemd drop-in kept in the repo,
[`deploy/host/phoneborg-pcprov.service.d/tvbox-scan.conf`](../deploy/host/phoneborg-pcprov.service.d/tvbox-scan.conf).
The extra slimming beyond `pcprov slim`, recorded in the slim state file so
`pcprov unslim` restores it too:

```sh
S=192.168.44.213:5555
P="com.google.android.apps.mediashell com.android.vending com.google.android.tvrecommendations
   com.nvidia.stats com.nvidia.NvCPLUpdater com.google.android.apps.nbu.smartconnect.tv"
for p in $P; do adb -s $S shell pm disable-user --user 0 $p; done
adb -s $S shell "cd /data/local/tmp/phoneborg && (cat slim.state; printf '%s\n' $P) | sort -u > slim.state.new && mv slim.state.new slim.state"
adb -s $S shell device_config put activity_manager max_cached_processes 4
```

Disabling the Play Store stops app updates and installs; re-enable it with
`pcprov unslim` when needed. `com.nvidia.nvgamecast` is persistent and keeps
running (about 9 MB) until the next reboot.

Big Brain: a Big Brain slice is the node's free RAM minus 900 MiB, so the
Shield would add only about 0.7 GB of layers, and every extra node slows
every token. A 3 GB box is worth more as a fast small-model worker
(`pool/fast`); boxes with 8–16 GB are the Big Brain candidates.

## NVIDIA Shield TV Pro (2019): expectations before the test

- **SoC:** Tegra X1+, 4× Cortex-A57 (ARMv8.0, no dotprod). pcprov will pick the
  slowest llama.cpp build, `armv8-a`.
- **RAM:** 3 GB; about 1.5–2 GB is left after Android TV. Expect 0.5B–1.5B
  models, not 4B.
- **Connection:** the 2019 Pro has only USB-A host ports, so adb runs over the
  network: `pcprov watch -connect <ip>:5555`. Use Ethernet, not Wi-Fi.
- **Good:** solid cooling and power; the serial and 64-bit userland should be
  fine (verify: `adb shell getprop ro.product.cpu.abilist`, `uname -m`).
- **Unknown:** tok/s; no published llama.cpp numbers for the Shield were found.

## What to look for when buying

| Criterion | Why |
|---|---|
| **Real** 4 GB RAM, better 8 GB | cheap boxes often ship less than advertised; `pbctl nodes` shows the truth |
| 64-bit Android and a 64-bit kernel | a box whose kernel reports `armv7l` cannot run the arm64 agent at all |
| Cortex-A55 or A76 cores | dotprod + fp16 enable the faster llama.cpp builds; A53 is the slowest |
| Cooling | long generations throttle without a heatsink; the gateway avoids nodes at ≥ 75 °C |
| Ethernet port | stable network adb |

## Options

| Option | Price (listings) | Notes |
|---|---|---|
| RK3588 box, 8 GB, with a fan (e.g. H96 Max V58) | ~$180–280 | fastest TV box (A76 cores); no independent ranking found, read reviews for throttling |
| Budget RK3566 / S905X4 box, 4 GB | cheap | slow, needs extra cooling; useful for pools of small models or as RAM for Big Brain |
| RK3588 SBC (Orange Pi 5, Rock 5) on Linux | — | **works today without code changes**: run `llama-server` and add it as an external node; good cooling, up to 16 GB |
| Used phone with 8 GB | — | often more tok/s per dollar; the battery acts as a UPS; the USB path already works |

Buy **one** unit of a model first, measure it, then buy more: specs on cheap
boxes are often fake.

### Cooling the RK3566 box

Open or drill the case, stick a heatsink on the SoC and add a small 5 V fan
powered from the box's own USB port. Without it tok/s drop after a few minutes.

## Network discovery

Boxes without a USB device port (the Shield) run adb over the network.
`pcprov watch -scan` finds them, so a box can be added by just enabling
network debugging on it:

```sh
pcprov watch -model models/qwen2.5-0.5b-instruct-q4_k_m.gguf \
  -scan 192.168.50.0/24          # also: 192.168.50.20, 192.168.50.20:5556,
                                 #       192.168.50.10-40, 192.168.50.10-192.168.50.40
```

- Every `-scan-interval` (default `2m`) pcprov TCP-probes each target on
  `-scan-port` (default `5555`, 800 ms timeout, 64 in parallel) and runs
  `adb connect` on the open ones that are not already connected. Devices in
  `offline` state are reconnected; `unauthorized` ones are left alone until
  "Allow network debugging" is accepted on the TV with the remote.
- The watch loop then sees `<ip>:5555` in `adb devices` and provisions it
  like a USB phone (backoff, Devices panel, auto-provision toggle).
- `-scan` is repeatable and adds to USB detection; with `-serial`/`-connect`
  the scanned hosts are managed on top of that allow-list. A spec that expands
  to more than 4096 hosts, IPv6 and malformed specs are rejected at start.
- A box reachable over both USB and the network shows up twice under two
  serials; use one path per box.
- `pcprov slim -serial <ip>:5555` (or `watch -slim`) also knows the TV apps:
  streaming (Netflix, Prime Video, YouTube, Plex, ...), games, Live TV, the
  Assistant and NVIDIA's NAS/SMB server. Launcher, keyboard, Play services,
  Chromecast and remote pairing stay; `pcprov unslim` restores everything.
  On the Shield it freed 169 MiB (MemAvailable 899 → 1068 MiB).
- Scan only the boxes' own VLAN (row 4 below): anything there with adb open
  gets connected and, once authorized, provisioned.

## Code changes

| # | Problem | Where | Fix |
|---|---|---|---|
| 1 | Many cheap boxes have a 64-bit CPU and kernel but a 32-bit Android userland: `abilist` lacks `arm64-v8a`, so provisioning rejects them | `provisioner/provision.go:59-64` (`SupportedABI` at `:38-41`) | Done: `SupportedArch` falls back to `uname -m`; a 32-bit userland is logged as a warning. llama variant selection reads `/proc/cpuinfo`, which on a 32-bit userland lists 32-bit feature names, so such a box gets the baseline `armv8-a` build. Still needs a test on a real box |
| 2 | Cheap boxes often report the placeholder serial `0123456789ABCDEF`. Over USB pcprov refuses them; over network adb every box would get the **same** node ID | `node-agent/inventory.go:236` (`NodeID`), placeholder list in `provisioner/devices.go:39` | Done: `pickNodeID` skips placeholder serials and falls back to `android_id`, then hostname |
| 3 | Boxes on network adb are not found by the USB hot-plug watch | `pcprov watch` (`-connect` in `provisioner/cmd/pcprov/main.go:57`) | Done: `pcprov watch -scan <ip, CIDR or range>` probes the targets every `-scan-interval` (2 min) and runs `adb connect` on new hosts with the adb port open, see [Network discovery](#network-discovery) |
| 4 | Network adb is an open door | docs | put boxes on their own VLAN, never expose port 5555 beyond it, keep adb key authorization on |

Already fine, no change needed:

- no battery: `ParseBatteryDumpsys` returns nil values (`node-agent/inventory.go:73-92`);
- temperature comes from `/sys/class/thermal/thermal_zone*` (`MaxTemperature`, `:212-227`);
- the agent and llama.cpp are static binaries with no dependency on the Android userland;
- adb reverse and forward work over a network adb connection as over USB.

Out of scope: boxes with a 32-bit kernel (`armv7l`) would need armv7 builds of
the agent and llama.cpp, and would be too slow to be worth it.

## Test plan (Shield first)

1. `adb connect <ip>:5555`, then check `getprop ro.product.cpu.abilist`,
   `uname -m`, `getprop ro.serialno`.
2. `pcprov provision` / `pcprov watch -connect <ip>:5555`; the box should reach
   `ACTIVE` in `pbctl nodes`.
3. Measure with `pbctl mmb run nodes=<alias>`: tok/s for 0.5B and 1.5B, and the
   temperature after 10 minutes of load. Done for 0.35B–1B, see above;
   temperature under sustained load is still open.
4. Add the box to a pool and to a Big Brain test, then record the numbers in
   `docs/BENCHMARKS.md` and turn this note into a "TV boxes" section in
   `docs/REAL_PHONES.md`.

## Sources

- [AI models may be capable of self-replicating without access to a datacenter](https://www.anthropic.com/research/self-replicating-ai) (Anthropic, 2026-10-12)
- [RK3588 Android TV Box Buyer's Guide 2026 (Alibaba)](https://electronics.alibaba.com/product/rockchip-rk3588-android-tv-box)
- [RK3588 TV Box Guide: What you actually need (Alibaba)](https://electronics.alibaba.com/buyingguides/rk3588-tv-box-guide-what-you-actually-need)
- [H96 MAX V58 RK3588 (eBay)](https://www.ebay.com/itm/385594589078)
- [RK3588 TV box listings (AliExpress)](https://www.aliexpress.com/w/wholesale-rockchip-rk3588-tv-box.html)
- [llama.cpp Android docs (mirror)](https://huggingface.co/spaces/Steven10429/apply_lora_and_quantize/blob/999165ea0520b95c7138d1319941f2402f8d5b51/llama.cpp/docs/android.md?code=true)
