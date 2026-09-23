// Package nodeagent discovers device capabilities and talks to the controller.
// It runs as the adb `shell` user, so every probe must tolerate missing or
// SELinux-denied files and degrade to "unknown" instead of failing.
package nodeagent

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"syscall"

	"github.com/dzaczek/phoneborg/proto"
)

var propLine = regexp.MustCompile(`^\[([^\]]+)\]: \[(.*)\]$`)

// ParseGetprop parses `getprop` output ("[key]: [value]" per line).
func ParseGetprop(out string) map[string]string {
	props := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		if m := propLine.FindStringSubmatch(strings.TrimSpace(line)); m != nil {
			props[m[1]] = m[2]
		}
	}
	return props
}

// ParseMeminfo returns /proc/meminfo values in bytes, keyed by field name.
func ParseMeminfo(out string) map[string]uint64 {
	m := map[string]uint64{}
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		v, err := strconv.ParseUint(f[1], 10, 64)
		if err != nil {
			continue
		}
		if len(f) > 2 && f[2] == "kB" {
			v *= 1024
		}
		m[strings.TrimSuffix(f[0], ":")] = v
	}
	return m
}

// ParseCgroupMemMax parses cgroup v2 memory.max; ok=false for "max" or garbage.
func ParseCgroupMemMax(s string) (uint64, bool) {
	v, err := strconv.ParseUint(strings.TrimSpace(s), 10, 64)
	return v, err == nil && v > 0
}

// ParseCgroupCPUMax parses cgroup v2 cpu.max ("quota period"), rounding the
// quota up to whole cores. ok=false when unlimited.
func ParseCgroupCPUMax(s string) (int, bool) {
	f := strings.Fields(s)
	if len(f) != 2 || f[0] == "max" {
		return 0, false
	}
	q, err1 := strconv.ParseFloat(f[0], 64)
	p, err2 := strconv.ParseFloat(f[1], 64)
	if err1 != nil || err2 != nil || p <= 0 || q <= 0 {
		return 0, false
	}
	return int((q + p - 1) / p), true
}

// ParseBatteryDumpsys extracts level (%) and temperature (°C) from
// `dumpsys battery`. Values are nil when no battery is present.
func ParseBatteryDumpsys(out string) (level *int, tempC *float64) {
	kv := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		if k, v, ok := strings.Cut(strings.TrimSpace(line), ":"); ok {
			kv[k] = strings.TrimSpace(v)
		}
	}
	if kv["present"] != "true" {
		return nil, nil
	}
	if l, err := strconv.Atoi(kv["level"]); err == nil {
		level = &l
	}
	if t, err := strconv.Atoi(kv["temperature"]); err == nil && t > 0 {
		c := float64(t) / 10 // reported in tenths of °C
		tempC = &c
	}
	return level, tempC
}

// ParseThermalMilli converts a thermal_zone temp reading to °C. Most kernels
// report millidegrees; some report degrees. Implausible values are rejected.
func ParseThermalMilli(s string) (float64, bool) {
	v, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return 0, false
	}
	if v > 1000 || v < -1000 {
		v /= 1000
	}
	return v, v > 0 && v < 150
}

func readFile(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(b)
}

func run(name string, args ...string) string {
	out, err := exec.Command(name, args...).Output()
	if err != nil {
		return ""
	}
	return string(out)
}

// Props returns system properties; empty off-Android.
func Props() map[string]string { return ParseGetprop(run("getprop")) }

func Discover(props map[string]string, version string) proto.Inventory {
	inv := proto.Inventory{
		Manufacturer:   props["ro.product.manufacturer"],
		Model:          props["ro.product.model"],
		SoC:            firstNonEmpty(props["ro.soc.model"], props["ro.board.platform"], props["ro.hardware"]),
		ABI:            firstNonEmpty(props["ro.product.cpu.abi"], runtime.GOARCH),
		AndroidRelease: props["ro.build.version.release"],
		CPUCores:       runtime.NumCPU(),
		RAMTotalBytes:  ParseMeminfo(readFile("/proc/meminfo"))["MemTotal"],
		AgentVersion:   version,
	}
	inv.SDK, _ = strconv.Atoi(props["ro.build.version.sdk"])
	// Respect container limits so emulated low-end phones report what they
	// actually get. On real phones these files report "max" and are ignored.
	if lim, ok := ParseCgroupMemMax(readFile("/sys/fs/cgroup/memory.max")); ok && lim < inv.RAMTotalBytes {
		inv.RAMTotalBytes = lim
	}
	if c, ok := ParseCgroupCPUMax(readFile("/sys/fs/cgroup/cpu.max")); ok && c < inv.CPUCores {
		inv.CPUCores = c
	}
	var st syscall.Statfs_t
	if syscall.Statfs(WorkDir(), &st) == nil {
		inv.StorageFreeBytes = st.Bavail * uint64(st.Bsize)
	}
	return inv
}

// AvailableRAM is MemAvailable, capped by remaining cgroup headroom.
func AvailableRAM() uint64 {
	avail := ParseMeminfo(readFile("/proc/meminfo"))["MemAvailable"]
	if lim, ok := ParseCgroupMemMax(readFile("/sys/fs/cgroup/memory.max")); ok {
		cur, err := strconv.ParseUint(strings.TrimSpace(readFile("/sys/fs/cgroup/memory.current")), 10, 64)
		if err == nil && cur < lim && lim-cur < avail {
			avail = lim - cur
		}
	}
	return avail
}

func Load1() float64 {
	f := strings.Fields(readFile("/proc/loadavg"))
	if len(f) == 0 {
		return 0
	}
	v, _ := strconv.ParseFloat(f[0], 64)
	return v
}

var thermalZoneType = regexp.MustCompile(`(?i)cpu|gpu|kryo|skin|tsens`)

// ThermalZoneRelevant reports whether a thermal zone type measures silicon or
// skin temperature. Phones also expose voltages (vbat), currents (ibat),
// charge level (soc) and fixed throttling thresholds (lmh-dcvs) as zones.
func ThermalZoneRelevant(zoneType string) bool {
	return thermalZoneType.MatchString(zoneType)
}

// MaxTemperature returns the hottest CPU/GPU/skin thermal zone, falling back
// to battery temperature. nil when nothing is readable (e.g. redroid).
func MaxTemperature(batteryTemp *float64) *float64 {
	var best *float64
	zones, _ := filepath.Glob("/sys/class/thermal/thermal_zone*")
	for _, z := range zones {
		if !ThermalZoneRelevant(strings.TrimSpace(readFile(z + "/type"))) {
			continue
		}
		if v, ok := ParseThermalMilli(readFile(z + "/temp")); ok && (best == nil || v > *best) {
			best = &v
		}
	}
	if best == nil {
		return batteryTemp
	}
	return best
}

func Battery() (*int, *float64) { return ParseBatteryDumpsys(run("dumpsys", "battery")) }

// NodeID prefers the hardware serial, then Android ID (stable per device and
// readable by the shell user), then hostname.
func NodeID(props map[string]string) string {
	id := firstNonEmpty(props["ro.serialno"], strings.TrimSpace(run("settings", "get", "secure", "android_id")))
	if id == "" || id == "null" {
		id, _ = os.Hostname()
	}
	return sanitize(id)
}

func WorkDir() string {
	if d, err := os.Getwd(); err == nil {
		return d
	}
	return "/"
}

func sanitize(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r == '-' || r == '_' || r == '.' || (r >= '0' && r <= '9') || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
