package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/dzaczek/phoneborg/proto"
)

// postReport seeds the controller's device view directly, the way pcprov's
// -admin-token-file reporting would (ADR-019); pbctl itself has no "report"
// command, only "devices"/"devices auto"/"devices provision".
func postReport(t *testing.T, url string, devs []proto.DeviceStatus) {
	t.Helper()
	body, _ := json.Marshal(proto.DeviceReport{Host: "adbhost", Devices: devs})
	req, _ := http.NewRequest(http.MethodPost, url+"/admin/devices/report", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("seed report: HTTP %d", resp.StatusCode)
	}
}

func TestDevicesCommands(t *testing.T) {
	ts := newController(t)
	env := map[string]string{"PHONEBORG_URL": ts.URL, "PHONEBORG_ADMIN_TOKEN": token}

	if stdout, _, code := pbctl(t, env, "devices"); code != 0 || !strings.Contains(stdout, "no devices detected") {
		t.Fatalf("empty devices: %d %s", code, stdout)
	}

	postReport(t, ts.URL, []proto.DeviceStatus{
		{Serial: "S1", ADBState: "unauthorized", Status: proto.DeviceWaitingAuth, Hint: "accept the prompt"},
	})

	for _, tc := range []struct {
		args []string
		want []string
	}{
		{[]string{"devices"}, []string{"SERIAL", "S1", "unauthorized", "waiting-authorization", "accept the prompt"}},
		{[]string{"devices", "auto", "off"}, []string{"auto-provision off"}},
		{[]string{"devices"}, []string{"auto-provision: off"}},
		{[]string{"devices", "auto", "on"}, []string{"auto-provision on"}},
		{[]string{"devices", "provision", "S1"}, []string{"provision requested for S1"}},
		{[]string{"devices"}, []string{"queued"}},
	} {
		stdout, stderr, code := pbctl(t, env, tc.args...)
		if code != 0 {
			t.Fatalf("%v: exit %d: %s", tc.args, code, stderr)
		}
		for _, w := range tc.want {
			if !strings.Contains(stdout, w) {
				t.Errorf("%v: output lacks %q:\n%s", tc.args, w, stdout)
			}
		}
	}

	if stdout, _, code := pbctl(t, env, "devices", "-json"); code != 0 || !strings.Contains(stdout, `"serial": "S1"`) {
		t.Fatalf("json output: %d %s", code, stdout)
	}

	for _, tc := range []struct {
		args []string
		code int
		want string
	}{
		{[]string{"devices", "auto", "sideways"}, 2, "usage:"},
		{[]string{"devices", "provision"}, 2, "usage:"},
	} {
		_, stderr, code := pbctl(t, env, tc.args...)
		if code != tc.code || !strings.Contains(stderr, tc.want) {
			t.Errorf("%v: exit %d, stderr %q; want %d, %q", tc.args, code, stderr, tc.code, tc.want)
		}
	}
}
