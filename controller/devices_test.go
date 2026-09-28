package controller

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dzaczek/phoneborg/proto"
)

// newDeviceEnv is newEnv with a devices settings file (ADR-019).
func newDeviceEnv(t *testing.T, dir string, autoProvision bool, nodes ...string) *env {
	t.Helper()
	dev := DeviceOptions{AutoProvision: autoProvision}
	if dir != "" {
		dev.File = filepath.Join(dir, "devices.json")
		var err error
		if dev.AutoProvision, err = LoadDeviceSettings(dev.File); err != nil {
			t.Fatal(err)
		}
	}
	e := newEnv(t, testToken, nil, nodes...)
	e.srv.devices = newDevices(dev.File, dev.AutoProvision)
	return e
}

func reportBody(host string, devs []proto.DeviceStatus, acked ...string) string {
	b, _ := json.Marshal(proto.DeviceReport{Host: host, Devices: devs, Acked: acked})
	return string(b)
}

func TestAdminDevicesRequiresToken(t *testing.T) {
	e := newDeviceEnv(t, "", true)
	for _, req := range []struct {
		method, path string
	}{
		{http.MethodGet, "/admin/devices"},
		{http.MethodPut, "/admin/devices"},
		{http.MethodPost, "/admin/devices/report"},
		{http.MethodPost, "/admin/devices/S1/provision"},
	} {
		if w := e.do(req.method, req.path, "", "wrong-token"); w.Code != http.StatusUnauthorized {
			t.Fatalf("%s %s without a valid token: %d", req.method, req.path, w.Code)
		}
	}
}

func TestDevicesReportAndView(t *testing.T) {
	e := newDeviceEnv(t, "", true, "S1") // "S1" is both the reporting serial and a registered node id
	now := time.Now().UTC().Truncate(time.Second)
	body := reportBody("adbhost", []proto.DeviceStatus{
		{Serial: "S1", ADBState: "device", Model: "Pixel_6", Status: proto.DeviceProvisioned, FirstSeen: now, LastSeen: now},
		{Serial: "S2", ADBState: "unauthorized", Status: proto.DeviceWaitingAuth, Hint: "accept the prompt", FirstSeen: now, LastSeen: now},
	})
	var resp proto.DeviceReportResponse
	e.admin(http.MethodPost, "/admin/devices/report", body, 200, &resp)
	if !resp.AutoProvision || len(resp.Commands) != 0 {
		t.Fatalf("report response = %+v", resp)
	}

	var ds Devices
	e.admin(http.MethodGet, "/admin/devices", "", 200, &ds)
	if !ds.AutoProvision || len(ds.Devices) != 2 {
		t.Fatalf("devices = %+v", ds)
	}
	byS := map[string]DeviceView{}
	for _, d := range ds.Devices {
		byS[d.Serial] = d
	}
	s1 := byS["S1"]
	if s1.Host != "adbhost" || s1.Status != proto.DeviceProvisioned || s1.Stale || s1.NodeID != "S1" {
		t.Fatalf("S1 = %+v", s1)
	}
	s2 := byS["S2"]
	if s2.ADBState != "unauthorized" || s2.Hint != "accept the prompt" || s2.NodeID != "" {
		t.Fatalf("S2 = %+v", s2)
	}

	// A second report from the same host replaces its device list (S2 gone).
	e.admin(http.MethodPost, "/admin/devices/report",
		reportBody("adbhost", []proto.DeviceStatus{{Serial: "S1", ADBState: "device", Status: proto.DeviceProvisioned, FirstSeen: now, LastSeen: now}}),
		200, nil)
	e.admin(http.MethodGet, "/admin/devices", "", 200, &ds)
	if len(ds.Devices) != 1 || ds.Devices[0].Serial != "S1" {
		t.Fatalf("after replace = %+v", ds)
	}
}

func TestDevicesStaleness(t *testing.T) {
	e := newDeviceEnv(t, "", true)
	e.admin(http.MethodPost, "/admin/devices/report",
		reportBody("adbhost", []proto.DeviceStatus{{Serial: "S1", ADBState: "device", Status: proto.DeviceNew}}), 200, nil)

	var ds Devices
	e.admin(http.MethodGet, "/admin/devices", "", 200, &ds)
	if len(ds.Devices) != 1 || ds.Devices[0].Stale {
		t.Fatalf("fresh report shown stale: %+v", ds.Devices)
	}

	// Backdate the host's last report past DeviceStaleAfter.
	e.srv.devices.mu.Lock()
	e.srv.devices.byHost["adbhost"].lastSeen = time.Now().Add(-DeviceStaleAfter - time.Second)
	e.srv.devices.mu.Unlock()

	e.admin(http.MethodGet, "/admin/devices", "", 200, &ds)
	if len(ds.Devices) != 1 || !ds.Devices[0].Stale {
		t.Fatalf("quiet host not shown stale: %+v", ds.Devices)
	}
}

func TestDevicesProvisionNowDeliveryAndAck(t *testing.T) {
	e := newDeviceEnv(t, "", false) // auto-provision off: an operator drives it
	report := func(acked ...string) proto.DeviceReportResponse {
		var resp proto.DeviceReportResponse
		e.admin(http.MethodPost, "/admin/devices/report",
			reportBody("adbhost", []proto.DeviceStatus{{Serial: "S1", ADBState: "device", Status: proto.DeviceNew}}, acked...), 200, &resp)
		return resp
	}
	if resp := report(); resp.AutoProvision || len(resp.Commands) != 0 {
		t.Fatalf("before any request: %+v", resp)
	}

	var accepted map[string]any
	e.admin(http.MethodPost, "/admin/devices/S1/provision", "", http.StatusAccepted, &accepted)
	if accepted["serial"] != "S1" || accepted["provision_requested"] != true {
		t.Fatalf("provision response = %+v", accepted)
	}
	// Idempotent: asking again before it is acknowledged changes nothing.
	e.admin(http.MethodPost, "/admin/devices/S1/provision", "", http.StatusAccepted, nil)

	var ds Devices
	e.admin(http.MethodGet, "/admin/devices", "", 200, &ds)
	if len(ds.Devices) != 1 || !ds.Devices[0].Pending {
		t.Fatalf("pending not shown: %+v", ds.Devices)
	}

	resp := report() // pcprov's next poll sees the queued command, does not ack yet
	if len(resp.Commands) != 1 || resp.Commands[0].Serial != "S1" {
		t.Fatalf("commands = %+v", resp.Commands)
	}
	resp = report() // still not acked: redelivered
	if len(resp.Commands) != 1 || resp.Commands[0].Serial != "S1" {
		t.Fatalf("command not redelivered: %+v", resp.Commands)
	}

	resp = report("S1") // pcprov executed it and acknowledges
	if len(resp.Commands) != 0 {
		t.Fatalf("acked command still pending: %+v", resp.Commands)
	}
	e.admin(http.MethodGet, "/admin/devices", "", 200, &ds)
	if len(ds.Devices) != 1 || ds.Devices[0].Pending {
		t.Fatalf("pending not cleared: %+v", ds.Devices)
	}
}

func TestDevicesAutoProvisionSetAndPersistence(t *testing.T) {
	dir := t.TempDir()
	e := newDeviceEnv(t, dir, true)

	var ds Devices
	e.admin(http.MethodPut, "/admin/devices", `{"auto_provision":false}`, 200, &ds)
	if ds.AutoProvision {
		t.Fatalf("auto_provision not cleared: %+v", ds)
	}
	e.admin(http.MethodPut, "/admin/devices", `{}`, http.StatusBadRequest, nil)
	e.admin(http.MethodPut, "/admin/devices", `{"auto_provision":true,"extra":1}`, http.StatusBadRequest, nil)

	file := filepath.Join(dir, "devices.json")
	fi, err := os.Stat(file)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("file mode %v %v", fi, err)
	}
	if data, _ := os.ReadFile(file); !strings.Contains(string(data), `"auto_provision": false`) {
		t.Fatalf("setting not persisted: %s", data)
	}

	// A fresh controller reads the persisted setting back.
	e2 := newDeviceEnv(t, dir, true)
	e2.admin(http.MethodGet, "/admin/devices", "", 200, &ds)
	if ds.AutoProvision {
		t.Fatalf("persisted setting not loaded: %+v", ds)
	}

	if !strings.Contains(e.logs.String(), `"action":"devices_set"`) {
		t.Fatal("devices_set not audited")
	}
}

func TestLoadDeviceSettingsDefaultsAndErrors(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "nope.json")
	if on, err := LoadDeviceSettings(missing); err != nil || !on {
		t.Fatalf("missing file: %v %v, want true, nil", on, err)
	}

	bad := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(bad, []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadDeviceSettings(bad); err == nil {
		t.Fatal("corrupt file accepted")
	}

	wrongVersion := filepath.Join(dir, "v2.json")
	if err := os.WriteFile(wrongVersion, []byte(`{"version":2,"auto_provision":false}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadDeviceSettings(wrongVersion); err == nil {
		t.Fatal("unsupported version accepted")
	}
}
