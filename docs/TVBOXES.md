# Android TV boxes as PhoneBorg nodes

Working note, 2026-10-20: hardware options and the code changes needed to run
Android TV boxes next to the phones. Nothing here has been tested on a real
box yet; performance figures are estimates until measured.

## Why

TV boxes are cheap, always on mains power and do not sleep like phones.
Anthropic's red team ran a 26B model split across 15 off-brand Android TV
boxes (~$55 each) with the same layer-split idea as Big Brain
([study](https://www.anthropic.com/research/self-replicating-ai)). The same
study is also the warning: cheap, poorly secured devices with network adb are
exactly the attack surface it describes.

## NVIDIA Shield TV Pro (ordered, first test device)

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

## Code changes

| # | Problem | Where | Fix |
|---|---|---|---|
| 1 | Many cheap boxes have a 64-bit CPU and kernel but a 32-bit Android userland: `abilist` lacks `arm64-v8a`, so provisioning rejects them | `provisioner/provision.go:59-64` (`SupportedABI` at `:38-41`) | if `abilist` has no `arm64-v8a`, accept the device when `uname -m` is `aarch64`; the agent (`CGO_ENABLED=0`) and llama.cpp are static arm64 binaries and should run there. Needs a test on a real box |
| 2 | Cheap boxes often report the placeholder serial `0123456789ABCDEF`. Over USB pcprov refuses them; over network adb every box would get the **same** node ID | `node-agent/inventory.go:236` (`NodeID`), placeholder list in `provisioner/devices.go:39` | skip placeholder serials in `NodeID` and fall back to `android_id` |
| 3 | Boxes on network adb are not found by the USB hot-plug watch | `pcprov watch` (`-connect` in `provisioner/cmd/pcprov/main.go:57`) | document `-connect <ip>:5555`; optionally reconnect dropped network devices on each watch tick |
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
   temperature after 10 minutes of load.
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
