package provisioner

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// llamaVariant is one llama.cpp build in the ARM_ARCH sense (see
// runtime/llama/Dockerfile and ADR-007), ordered fastest first.
type llamaVariant struct {
	Name     string   // subdir of the -llama-dir tree, e.g. "armv8.2-a+dotprod+fp16"
	Requires []string // /proc/cpuinfo Features entries all required
}

// LlamaVariants lists the builds pcprov can choose between, fastest first.
var LlamaVariants = []llamaVariant{
	{Name: "armv8.2-a+dotprod+fp16", Requires: []string{"asimddp", "asimdhp"}},
	{Name: "armv8.2-a+fp16", Requires: []string{"asimdhp"}},
	{Name: "armv8-a", Requires: nil},
}

// ParseCPUFeatures parses the first "Features"/"features" line of
// /proc/cpuinfo into a set membership map.
func ParseCPUFeatures(cpuinfo string) map[string]bool {
	features := map[string]bool{}
	for _, line := range strings.Split(cpuinfo, "\n") {
		name, val, ok := strings.Cut(line, ":")
		if !ok || !strings.EqualFold(strings.TrimSpace(name), "features") {
			continue
		}
		for _, f := range strings.Fields(val) {
			features[f] = true
		}
		break
	}
	return features
}

// SelectLlamaVariant picks the fastest llama.cpp build whose CPU requirements
// are met by features and which is present in available (as returned by
// AvailableLlamaVariants).
func SelectLlamaVariant(features map[string]bool, available []string) (string, error) {
	have := map[string]bool{}
	for _, v := range available {
		have[v] = true
	}
	for _, v := range LlamaVariants {
		met := true
		for _, req := range v.Requires {
			if !features[req] {
				met = false
				break
			}
		}
		if met && have[v.Name] {
			return v.Name, nil
		}
	}
	var haveFeatures []string
	for f := range features {
		haveFeatures = append(haveFeatures, f)
	}
	sort.Strings(haveFeatures)
	return "", fmt.Errorf("no llama.cpp build matches this phone's CPU: phone features=%v, available builds=%v (build one with `make llama-all`)", haveFeatures, available)
}

// AvailableLlamaVariants lists the subdirectories of dir that contain an
// executable llama-server, i.e. the variants a previous `make llama`/`make
// llama-all` produced.
func AvailableLlamaVariants(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		fi, err := os.Stat(filepath.Join(dir, e.Name(), "llama-server"))
		if err != nil || fi.IsDir() || fi.Mode()&0o111 == 0 {
			continue
		}
		out = append(out, e.Name())
	}
	return out
}
