package nodeagent

import (
	"fmt"
	"strconv"
	"strings"
)

// ParseCPUList parses a Linux CPU list as found in /proc/self/status'
// Cpus_allowed_list or /sys/devices/system/cpu/*/topology/core_siblings_list,
// e.g. "0-5", "0-3,6,7" or "4". Malformed entries are skipped.
func ParseCPUList(s string) []int {
	var out []int
	for _, part := range strings.Split(strings.TrimSpace(s), ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if lo, hi, ok := strings.Cut(part, "-"); ok {
			loN, err1 := strconv.Atoi(strings.TrimSpace(lo))
			hiN, err2 := strconv.Atoi(strings.TrimSpace(hi))
			if err1 != nil || err2 != nil || hiN < loN {
				continue
			}
			for i := loN; i <= hiN; i++ {
				out = append(out, i)
			}
			continue
		}
		n, err := strconv.Atoi(part)
		if err != nil {
			continue
		}
		out = append(out, n)
	}
	return out
}

// AllowedCPUs extracts and parses Cpus_allowed_list from the content of
// /proc/self/status. It reflects the Android cpuset the shell process is
// confined to, which is often narrower than the number of physical CPUs.
func AllowedCPUs(procStatus string) []int {
	for _, line := range strings.Split(procStatus, "\n") {
		key, val, ok := strings.Cut(line, ":")
		if ok && strings.TrimSpace(key) == "Cpus_allowed_list" {
			return ParseCPUList(val)
		}
	}
	return nil
}

// ChooseThreads picks a llama-server thread count from the CPUs the process
// is allowed to run on and their max frequencies (kHz), keyed by CPU index.
// bigCores is always the highest-frequency cluster among allowed (nil if no
// frequency data is available for any allowed CPU), so callers can log it
// regardless of policy.
//
// Policy "all" uses every allowed CPU (today's behavior). Policy "big" uses
// only the cores whose max frequency is at least 90% of the highest max
// frequency among allowed CPUs, falling back to every allowed CPU if no
// frequency data is available. An unknown policy is treated as "all".
func ChooseThreads(allowed []int, maxFreqKHz map[int]int, policy string) (threads int, bigCores []int) {
	bigCores = bigCoresOf(allowed, maxFreqKHz)
	if policy == "big" {
		if len(bigCores) > 0 {
			return len(bigCores), bigCores
		}
		return len(allowed), bigCores
	}
	return len(allowed), bigCores
}

// bigCoresOf returns the allowed CPUs whose max frequency is within 90% of
// the highest max frequency among allowed CPUs, or nil if none of the
// allowed CPUs have known frequency data.
func bigCoresOf(allowed []int, maxFreqKHz map[int]int) []int {
	highest := 0
	for _, c := range allowed {
		if f := maxFreqKHz[c]; f > highest {
			highest = f
		}
	}
	if highest == 0 {
		return nil
	}
	threshold := float64(highest) * 0.9
	var big []int
	for _, c := range allowed {
		if f, ok := maxFreqKHz[c]; ok && float64(f) >= threshold {
			big = append(big, c)
		}
	}
	return big
}

// DiscoverThreads reads the on-device CPU topology (allowed CPUs from
// /proc/self/status, max frequencies from sysfs cpufreq) and applies policy
// via ChooseThreads. Missing or SELinux-denied files degrade to "unknown"
// (readFile returns "") rather than failing: emulators have no cpufreq, and
// a shell process may not always be allowed to read every cpufreq node.
func DiscoverThreads(policy string) (threads int, allowed []int, bigCores []int) {
	allowed = AllowedCPUs(readFile("/proc/self/status"))
	freqs := make(map[int]int, len(allowed))
	for _, c := range allowed {
		path := fmt.Sprintf("/sys/devices/system/cpu/cpu%d/cpufreq/cpuinfo_max_freq", c)
		if v, err := strconv.Atoi(strings.TrimSpace(readFile(path))); err == nil && v > 0 {
			freqs[c] = v
		}
	}
	threads, bigCores = ChooseThreads(allowed, freqs, policy)
	return threads, allowed, bigCores
}
