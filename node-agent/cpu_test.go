package nodeagent

import (
	"reflect"
	"testing"
)

func TestParseCPUList(t *testing.T) {
	cases := map[string][]int{
		"0-5":     {0, 1, 2, 3, 4, 5},
		"0-3,6,7": {0, 1, 2, 3, 6, 7},
		"4":       {4},
		"":        nil,
	}
	for in, want := range cases {
		if got := ParseCPUList(in); !reflect.DeepEqual(got, want) {
			t.Fatalf("ParseCPUList(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestAllowedCPUs(t *testing.T) {
	status := "Name:\tnode-agent\nState:\tS (sleeping)\nCpus_allowed:\t3f\nCpus_allowed_list:\t0-5\nVmPeak:\t   12345 kB\n"
	if got, want := AllowedCPUs(status), []int{0, 1, 2, 3, 4, 5}; !reflect.DeepEqual(got, want) {
		t.Fatalf("AllowedCPUs = %v, want %v", got, want)
	}
	if got := AllowedCPUs("no such field here\n"); got != nil {
		t.Fatalf("AllowedCPUs with no field = %v, want nil", got)
	}
}

// mi8MaxFreq is the real max_freq topology of a Xiaomi Mi 8 (Snapdragon 845):
// cpu0-3 are the little cluster (1766400 kHz), cpu4-7 are the big cluster
// (2803200 kHz).
var mi8MaxFreq = map[int]int{
	0: 1766400, 1: 1766400, 2: 1766400, 3: 1766400,
	4: 2803200, 5: 2803200, 6: 2803200, 7: 2803200,
}

func TestChooseThreads_Mi8(t *testing.T) {
	cases := []struct {
		name        string
		allowed     []int
		freq        map[int]int
		policy      string
		wantThreads int
		wantBig     []int
	}{
		{"allowed 0-5, policy all", []int{0, 1, 2, 3, 4, 5}, mi8MaxFreq, "all", 6, []int{4, 5}},
		{"allowed 0-5, policy big", []int{0, 1, 2, 3, 4, 5}, mi8MaxFreq, "big", 2, []int{4, 5}},
		{"allowed 0-7, policy all", []int{0, 1, 2, 3, 4, 5, 6, 7}, mi8MaxFreq, "all", 8, []int{4, 5, 6, 7}},
		{"allowed 0-7, policy big", []int{0, 1, 2, 3, 4, 5, 6, 7}, mi8MaxFreq, "big", 4, []int{4, 5, 6, 7}},
		{"no cpufreq data, policy big falls back", []int{0, 1, 2, 3, 4, 5}, nil, "big", 6, nil},
		{"no cpufreq data, policy all", []int{0, 1, 2, 3, 4, 5}, nil, "all", 6, nil},
		{"unknown policy treated as all", []int{0, 1, 2, 3, 4, 5}, mi8MaxFreq, "bogus", 6, []int{4, 5}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			threads, big := ChooseThreads(c.allowed, c.freq, c.policy)
			if threads != c.wantThreads || !reflect.DeepEqual(big, c.wantBig) {
				t.Fatalf("got threads=%d big=%v, want threads=%d big=%v", threads, big, c.wantThreads, c.wantBig)
			}
		})
	}
}
