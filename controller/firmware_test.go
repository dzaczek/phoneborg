package controller

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/dzaczek/phoneborg/proto"
)

// Trimmed copies of the real sources as of 2026-10.
const pixelOTAFixture = `<table>
  <tr id="huskycp2a.260705.006">
    <td>17.0.0 (CP2A.260705.006, Jul 2026)</td>
    <td><a href="https://dl.google.com/dl/android/aosp/husky-ota-cp2a.260705.006-adca0996.zip">Link</a></td>
    <td>adca0996</td>
  </tr>
  <tr id="huskycp2a.260805.005">
    <td>17.0.0 (CP2A.260805.005, Aug 2026)</td>
    <td><a href="https://dl.google.com/dl/android/aosp/husky-ota-cp2a.260805.005-c88a25d5.zip">Link</a></td>
    <td>c88a25d5</td>
  </tr>
  <tr id="huskycp3a.260905.009">
    <td>17.0.0 (CP3A.260905.009, Sep 2026)</td>
    <td><a href="https://dl.google.com/dl/android/aosp/husky-ota-cp3a.260905.009-d674a136.zip">Link</a></td>
    <td>d674a136</td>
  </tr>
  <tr id="huskycp3a.260905.010">
    <td>17.0.0 (CP3A.260905.010, Sep 2026, Verizon)</td>
    <td><a href="https://dl.google.com/dl/android/aosp/husky-ota-cp3a.260905.010-0000.zip">Link</a></td>
    <td>0000</td>
  </tr>
</table>`

const xiaomiFixture = `- android: '13.0'
  branch: Stable
  codename: alioth
  date: 2025-11-02
  link: https://bigota.d.miui.com/OS1.0.10.0.TKHCNXM/x.zip
  method: Recovery
  name: Redmi K40 China
  version: OS1.0.10.0.TKHCNXM
- android: '13.0'
  branch: Stable
  codename: alioth_global
  date: 2026-01-12
  link: https://example.invalid/alioth_global_OS1.0.5.0.TKHMIXM.tgz
  method: Fastboot
  name: POCO F3 Global
  version: OS1.0.5.0.TKHMIXM
- android: '12.0'
  branch: Stable
  codename: alioth_global
  date: 2022-05-01
  link: https://example.invalid/old.zip
  version: V13.0.1.0.SKHMIXM
- android: '13.0'
  branch: Weekly
  codename: alioth
  date: 2026-05-01
  version: 26.5.1
`

const lineageFixture = `[{"date":"2026-09-26","version":"22.2","files":[{"url":"https://mirror.invalid/old.zip"}]},
 {"date":"2026-10-03","version":"22.2","files":[{"url":"https://mirror.invalid/lineage-22.2-20261003-nightly-dipper-signed.zip"}]}]`

func TestParsePixelOTA(t *testing.T) {
	got := parsePixelOTA([]byte(pixelOTAFixture))["husky"]
	if len(got) != 3 {
		t.Fatalf("husky builds = %+v, want 3 (the Verizon build skipped)", got)
	}
	if got[2].Build != "CP3A.260905.009" || got[2].Month != "Sep 2026" || !strings.HasSuffix(got[2].URL, "d674a136.zip") {
		t.Errorf("latest = %+v", got[2])
	}
}

func TestParseXiaomiTracker(t *testing.T) {
	got := parseXiaomiTracker([]byte(xiaomiFixture))
	if len(got) != 4 || got[1].Codename != "alioth_global" || got[1].Version != "OS1.0.5.0.TKHMIXM" || got[1].Android != "13.0" || got[3].Branch != "Weekly" {
		t.Fatalf("parsed = %+v", got)
	}
}

func TestParseLineageBuilds(t *testing.T) {
	b, err := parseLineageBuilds([]byte(lineageFixture))
	if err != nil || b == nil || b.Date != "2026-10-03" || !strings.Contains(b.URL, "20261003") {
		t.Fatalf("got %+v, %v", b, err)
	}
	if b, err := parseLineageBuilds([]byte(`[]`)); err != nil || b != nil {
		t.Fatalf("empty list: %+v, %v", b, err)
	}
}

func TestPatchAgeMonths(t *testing.T) {
	now := time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC)
	for patch, want := range map[string]int{"2026-09-01": 1, "2026-08-05": 2, "2024-03-01": 31, "2026-10-01": 0, "": -1, "bad": -1} {
		if got := patchAgeMonths(patch, now); got != want {
			t.Errorf("patchAgeMonths(%q) = %d, want %d", patch, got, want)
		}
	}
}

func testFirmwareData() firmwareData {
	return firmwareData{
		lineage: map[string]*lineageBuild{
			"dipper": {Version: "22.2", Date: "2026-10-03"},
			"alioth": {Version: "23.2", Date: "2026-09-30"},
			"husky":  {Version: "23.2", Date: "2026-10-02"},
		},
		pixel:  parsePixelOTA([]byte(pixelOTAFixture)),
		xiaomi: parseXiaomiTracker([]byte(xiaomiFixture)),
	}
}

func kinds(nf NodeFirmware) string {
	var out []string
	for _, u := range nf.Updates {
		out = append(out, u.Source+":"+u.Kind+":"+u.Version)
	}
	return strings.Join(out, ",")
}

// The four phones of the reference cluster, as their agents report them.
func TestEvalFirmware(t *testing.T) {
	now := time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC)
	data := testFirmwareData()
	for _, c := range []struct {
		name   string
		inv    proto.Inventory
		latest bool
		want   string
	}{
		{"mi8 on an older LineageOS nightly", proto.Inventory{Brand: "Xiaomi", Device: "dipper", LineageVersion: "22.2-20260919-NIGHTLY-dipper", SecurityPatch: "2026-09-01", Bootloader: "unlocked"},
			false, "lineageos:update:22.2"},
		{"mi8 on the newest nightly", proto.Inventory{Brand: "Xiaomi", Device: "dipper", LineageVersion: "22.2-20261003-NIGHTLY-dipper"},
			true, ""},
		{"mi8 on an older LineageOS version", proto.Inventory{Brand: "Xiaomi", Device: "dipper", LineageVersion: "21.0-20250101-NIGHTLY-dipper"},
			false, "lineageos:upgrade:22.2"},
		{"poco on old MIUI", proto.Inventory{Brand: "POCO", Device: "alioth", BuildIncremental: "V816.0.5.0.TKHMIXM", SecurityPatch: "2024-03-01", Bootloader: "locked"},
			false, "xiaomi:update:OS1.0.5.0.TKHMIXM,lineageos:alternative:23.2"},
		{"poco on the newest HyperOS", proto.Inventory{Brand: "POCO", Device: "alioth", BuildIncremental: "OS1.0.5.0.TKHMIXM", SecurityPatch: "2026-01-01"},
			true, "lineageos:alternative:23.2"},
		{"pixel one month behind", proto.Inventory{Brand: "google", Device: "husky", BuildID: "CP2A.260805.005", SecurityPatch: "2026-08-05", Bootloader: "locked"},
			false, "google:update:CP3A.260905.009,lineageos:alternative:23.2"},
		{"pixel up to date", proto.Inventory{Brand: "google", Device: "husky", BuildID: "CP3A.260905.009"},
			true, "lineageos:alternative:23.2"},
		{"oneplus: no source", proto.Inventory{Brand: "OnePlus", Device: "OP516FL1", BuildDisplay: "NE2213_16.0.3.530(EX01)", SecurityPatch: "2026-06-01", Bootloader: "locked"},
			false, ""},
	} {
		nf := evalFirmware("n", c.inv, data, now)
		if got := kinds(nf); got != c.want || nf.Latest != c.latest {
			t.Errorf("%s: updates %q latest %v, want %q latest %v", c.name, got, nf.Latest, c.want, c.latest)
		}
	}
	poco := evalFirmware("poco", proto.Inventory{Brand: "POCO", Device: "alioth", BuildIncremental: "V816.0.5.0.TKHMIXM", SecurityPatch: "2024-03-01", Bootloader: "locked"}, data, now)
	if poco.Current != "V816.0.5.0.TKHMIXM" || poco.PatchAgeMonths != 31 || !strings.Contains(poco.Updates[1].Note, "locked") {
		t.Errorf("poco = %+v", poco)
	}
	op := evalFirmware("op", proto.Inventory{Brand: "OnePlus", BuildDisplay: "NE2213_16.0.3.530(EX01)", BuildID: "X"}, data, now)
	if op.Current != "NE2213_16.0.3.530(EX01)" {
		t.Errorf("oneplus current = %q", op.Current)
	}
}

func TestFirmwareCheckFetchesWhatPhonesNeed(t *testing.T) {
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		switch {
		case r.URL.Path == "/lineage/dipper":
			io.WriteString(w, lineageFixture)
		case strings.HasPrefix(r.URL.Path, "/lineage/"):
			http.NotFound(w, r)
		case r.URL.Path == "/xiaomi":
			io.WriteString(w, xiaomiFixture)
		default:
			t.Errorf("unexpected fetch %s", r.URL.Path)
		}
	}))
	defer srv.Close()
	f := newFirmware(FirmwareOptions{Online: true, LineageURL: srv.URL + "/lineage/%s", PixelURL: srv.URL + "/pixel", XiaomiURL: srv.URL + "/xiaomi"},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	nodes := []proto.Node{
		{ID: "mi8", Inventory: proto.Inventory{Brand: "Xiaomi", Device: "dipper", LineageVersion: "22.2-20260919-NIGHTLY-dipper"}},
		{ID: "op", Inventory: proto.Inventory{Brand: "OnePlus", Device: "OP516FL1"}},
	}
	if !f.due(nodes) {
		t.Fatal("first check not due")
	}
	if err := f.check(context.Background(), nodes); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(paths, " "); !strings.Contains(got, "/lineage/dipper") || !strings.Contains(got, "/lineage/op516fl1") || !strings.Contains(got, "/xiaomi") || strings.Contains(got, "/pixel") {
		t.Errorf("fetched %q: want both codenames and the Xiaomi tracker, not Google's page", got)
	}
	if f.due(nodes) {
		t.Error("due again right after a check")
	}
	r := f.report(nodes, time.Now())
	if !r.Online || len(r.Nodes) != 2 || kinds(r.Nodes[0]) != "lineageos:update:22.2" || len(r.Errors) != 0 {
		t.Errorf("report = %+v", r)
	}
}

func TestFirmwareOffline(t *testing.T) {
	f := newFirmware(FirmwareOptions{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	r := f.report([]proto.Node{{ID: "a", Inventory: proto.Inventory{SecurityPatch: "2026-09-01", BuildID: "B1"}}}, time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC))
	if r.Online || len(r.Nodes) != 1 || r.Nodes[0].PatchAgeMonths != 1 || r.Nodes[0].Current != "B1" || len(r.Nodes[0].Updates) != 0 {
		t.Errorf("offline report = %+v", r)
	}
}

func TestFirmwareAdmin(t *testing.T) {
	e := newEnv(t, testToken, nil)
	e.reg.Register(proto.RegisterRequest{NodeID: "pixel", Inventory: proto.Inventory{Brand: "google", Device: "husky", BuildID: "CP2A.260805.005", SecurityPatch: "2026-08-05", Bootloader: "locked"}}, "x")
	var r FirmwareReport
	e.admin(http.MethodGet, "/admin/firmware", "", http.StatusOK, &r)
	if r.Online || len(r.Nodes) != 1 || r.Nodes[0].Current != "CP2A.260805.005" || r.Nodes[0].Bootloader != "locked" || r.Nodes[0].PatchAgeMonths < 0 {
		t.Errorf("report = %+v", r)
	}
	e.admin(http.MethodPost, "/admin/firmware/check", "", http.StatusConflict, nil) // offline: nothing to fetch
}
