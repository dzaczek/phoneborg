package provisioner

import "testing"

// mi8EnabledPackages mirrors what `cmd package list packages -e` reports on
// the Xiaomi Mi 8 (LineageOS 22.2) described in docs/REAL_PHONES.md: the
// slim-eligible apps plus a handful of packages that must never be touched.
var mi8EnabledPackages = []string{
	"package:org.lineageos.aperture",
	"package:org.lineageos.glimpse",
	"package:org.lineageos.twelve",
	"package:org.lineageos.jelly",
	"package:org.lineageos.etar",
	"package:com.android.deskclock",
	"package:org.lineageos.recorder",
	"package:com.android.messaging",
	"package:com.android.contacts",
	"package:com.android.dialer",
	"package:org.lineageos.setupwizard",
	"package:com.android.settings",
	"package:com.android.systemui",
	"package:com.android.phone",
	"package:com.android.launcher3",
	"",
	"",
}

func TestParseEnabledPackages(t *testing.T) {
	out := "package:org.lineageos.aperture\n" +
		"package:com.android.settings\n" +
		"\n" +
		"  package:com.android.deskclock  \n"
	got := ParseEnabledPackages(out)
	want := []string{"org.lineageos.aperture", "com.android.settings", "com.android.deskclock"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

func TestComputeDisableSet(t *testing.T) {
	var installed []string
	for _, p := range mi8EnabledPackages {
		installed = append(installed, ParseEnabledPackages(p)...)
	}

	t.Run("without telephony", func(t *testing.T) {
		got := ComputeDisableSet(installed, false)
		want := map[string]bool{
			"org.lineageos.aperture": true, "org.lineageos.glimpse": true,
			"org.lineageos.twelve": true, "org.lineageos.jelly": true,
			"org.lineageos.etar": true, "com.android.deskclock": true,
			"org.lineageos.recorder": true, "com.android.messaging": true,
		}
		if len(got) != len(want) {
			t.Fatalf("got %v", got)
		}
		for _, pkg := range got {
			if !want[pkg] {
				t.Errorf("unexpected package in disable set: %s", pkg)
			}
		}
		for _, never := range []string{"com.android.settings", "com.android.systemui", "com.android.phone", "com.android.launcher3", "org.lineageos.setupwizard", "com.android.contacts", "com.android.dialer"} {
			for _, pkg := range got {
				if pkg == never {
					t.Errorf("protected/telephony package %s must not be in the disable set", never)
				}
			}
		}
	})

	t.Run("with telephony", func(t *testing.T) {
		got := ComputeDisableSet(installed, true)
		hasContacts, hasDialer := false, false
		for _, pkg := range got {
			if pkg == "com.android.contacts" {
				hasContacts = true
			}
			if pkg == "com.android.dialer" {
				hasDialer = true
			}
		}
		if !hasContacts || !hasDialer {
			t.Fatalf("telephony=true should include dialer/contacts, got %v", got)
		}
	})

	t.Run("not installed apps are skipped", func(t *testing.T) {
		got := ComputeDisableSet([]string{"org.lineageos.aperture"}, false)
		if len(got) != 1 || got[0] != "org.lineageos.aperture" {
			t.Fatalf("got %v", got)
		}
	})

	t.Run("never disables a protected package even if it slipped into the table", func(t *testing.T) {
		if !isProtected("com.android.phone") {
			t.Error("com.android.phone must be protected")
		}
		for _, pkg := range []string{"com.android.systemui", "com.android.settings", "org.lineageos.setupwizard", "com.qualcomm.bluetoothfmapp", "com.android.providers.contacts", "com.android.permissioncontroller"} {
			if !isProtected(pkg) {
				t.Errorf("%s should be protected", pkg)
			}
		}
		if isProtected("org.lineageos.aperture") {
			t.Error("a legitimate slim target must not be protected")
		}
	})
}

func TestFindSetupWizard(t *testing.T) {
	installed := []string{"com.android.settings", "org.lineageos.setupwizard", "com.android.systemui"}
	got := FindSetupWizard(installed)
	if len(got) != 1 || got[0] != "org.lineageos.setupwizard" {
		t.Fatalf("got %v", got)
	}
	if got := FindSetupWizard([]string{"com.android.settings"}); len(got) != 0 {
		t.Fatalf("expected no setup wizard package, got %v", got)
	}
}

func TestSlimStateRoundTrip(t *testing.T) {
	pkgs := []string{"org.lineageos.aperture", "org.lineageos.glimpse"}
	data := FormatSlimState(pkgs)
	got := ParseSlimState(data)
	if len(got) != len(pkgs) {
		t.Fatalf("got %v, want %v", got, pkgs)
	}
	for i := range pkgs {
		if got[i] != pkgs[i] {
			t.Fatalf("got %v, want %v", got, pkgs)
		}
	}

	if FormatSlimState(nil) != "" {
		t.Fatal("empty state should format to an empty string")
	}
	if got := ParseSlimState("\n\n  \n"); got != nil {
		t.Fatalf("blank state file should parse to no packages, got %v", got)
	}
}

func TestMergeSlimState(t *testing.T) {
	t.Run("unions and dedupes", func(t *testing.T) {
		got := MergeSlimState([]string{"a", "b"}, []string{"b", "c"})
		want := []string{"a", "b", "c"}
		if len(got) != len(want) {
			t.Fatalf("got %v", got)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("got %v, want %v", got, want)
			}
		}
	})

	t.Run("running slim twice is idempotent", func(t *testing.T) {
		first := MergeSlimState(nil, []string{"org.lineageos.aperture", "com.android.deskclock"})
		// Second run: the packages are already disabled so ComputeDisableSet
		// would no longer see them among the enabled ones, so nothing new is
		// disabled; the recorded state must stay exactly the same.
		second := MergeSlimState(first, nil)
		if len(second) != len(first) {
			t.Fatalf("state changed on a no-op re-run: %v -> %v", first, second)
		}
		for i := range first {
			if first[i] != second[i] {
				t.Fatalf("state changed on a no-op re-run: %v -> %v", first, second)
			}
		}
	})
}

func TestParseMemAvailable(t *testing.T) {
	meminfo := "MemTotal:        8109408 kB\nMemFree:          123456 kB\nMemAvailable:    3993388 kB\n"
	v, ok := ParseMemAvailable(meminfo)
	if !ok || v != 3993388*1024 {
		t.Fatalf("got %d, %v", v, ok)
	}

	if _, ok := ParseMemAvailable("MemTotal: 8109408 kB\n"); ok {
		t.Fatal("expected ok=false when MemAvailable is absent")
	}

	if _, ok := ParseMemAvailable("MemAvailable: not-a-number kB\n"); ok {
		t.Fatal("expected ok=false for a malformed value")
	}
}
