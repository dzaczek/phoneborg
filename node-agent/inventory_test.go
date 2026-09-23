package nodeagent

import "testing"

func TestParseGetprop(t *testing.T) {
	p := ParseGetprop("[ro.product.model]: [Pixel 4a]\n[ro.build.version.sdk]: [31]\n[empty]: []\ngarbage\n")
	if p["ro.product.model"] != "Pixel 4a" || p["ro.build.version.sdk"] != "31" {
		t.Fatalf("got %v", p)
	}
	if v, ok := p["empty"]; !ok || v != "" {
		t.Fatalf("empty value not kept: %v", p)
	}
}

func TestParseMeminfo(t *testing.T) {
	m := ParseMeminfo("MemTotal:        8109408 kB\nMemAvailable:    3993388 kB\nHugePages_Total:       0\n")
	if m["MemTotal"] != 8109408*1024 || m["MemAvailable"] != 3993388*1024 || m["HugePages_Total"] != 0 {
		t.Fatalf("got %v", m)
	}
}

func TestParseCgroup(t *testing.T) {
	if v, ok := ParseCgroupMemMax("3221225472\n"); !ok || v != 3<<30 {
		t.Fatalf("mem limit: %d %v", v, ok)
	}
	if _, ok := ParseCgroupMemMax("max\n"); ok {
		t.Fatal("max must be unlimited")
	}
	for in, want := range map[string]int{"200000 100000\n": 2, "150000 100000": 2, "100000 100000": 1} {
		if got, ok := ParseCgroupCPUMax(in); !ok || got != want {
			t.Fatalf("cpu.max %q = %d,%v want %d", in, got, ok, want)
		}
	}
	if _, ok := ParseCgroupCPUMax("max 100000"); ok {
		t.Fatal("max must be unlimited")
	}
}

func TestParseBattery(t *testing.T) {
	lvl, temp := ParseBatteryDumpsys("Current Battery Service state:\n  present: true\n  level: 87\n  temperature: 312\n")
	if lvl == nil || *lvl != 87 || temp == nil || *temp != 31.2 {
		t.Fatalf("got %v %v", lvl, temp)
	}
	// redroid / emulator: no battery.
	if lvl, temp := ParseBatteryDumpsys("  present: false\n  level: 0\n"); lvl != nil || temp != nil {
		t.Fatalf("absent battery reported: %v %v", lvl, temp)
	}
}

func TestParseThermal(t *testing.T) {
	for in, want := range map[string]float64{"45000\n": 45, "38": 38} {
		if got, ok := ParseThermalMilli(in); !ok || got != want {
			t.Fatalf("%q = %v,%v", in, got, ok)
		}
	}
	for _, in := range []string{"-40000", "0", "999999", "x"} {
		if _, ok := ParseThermalMilli(in); ok {
			t.Fatalf("%q should be rejected", in)
		}
	}
}

func TestSanitize(t *testing.T) {
	if got := sanitize("ab:cd/ef 1-2_3.4"); got != "abcdef1-2_3.4" {
		t.Fatal(got)
	}
}
