package provisioner

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/dzaczek/phoneborg/proto"
)

// reportTimeout bounds one report call so a stuck controller never stalls
// Watch's poll loop for long.
const reportTimeout = 10 * time.Second

// Reporter posts pcprov's device view to the controller's admin API
// (docs/DECISIONS.md ADR-019) and returns the controller-side settings and
// pending operator commands. A nil *Reporter disables reporting entirely, so
// pcprov's existing CLI usage (no -admin-token-file) is unaffected.
type Reporter struct {
	URL    string // controller base URL, e.g. http://127.0.0.1:18080
	Token  string // admin token
	Host   string // this pcprov's identity, shown in the panel
	Client *http.Client
}

// NewReporter builds a Reporter identified by this host's hostname.
func NewReporter(controllerURL, token string) *Reporter {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "unknown-host"
	}
	return &Reporter{URL: strings.TrimRight(controllerURL, "/"), Token: token, Host: host, Client: &http.Client{Timeout: reportTimeout}}
}

// Report posts the current device view and the serials whose pending
// command was executed this cycle (see proto.DeviceReport.Acked).
func (r *Reporter) Report(ctx context.Context, devices []proto.DeviceStatus, acked []string) (proto.DeviceReportResponse, error) {
	var out proto.DeviceReportResponse
	body, err := json.Marshal(proto.DeviceReport{Host: r.Host, Devices: devices, Acked: acked})
	if err != nil {
		return out, err
	}
	ctx, cancel := context.WithTimeout(ctx, reportTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.URL+proto.PathDevicesReport, bytes.NewReader(body))
	if err != nil {
		return out, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+r.Token)
	resp, err := r.Client.Do(req)
	if err != nil {
		return out, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return out, err
	}
	if resp.StatusCode != http.StatusOK {
		return out, fmt.Errorf("POST %s: HTTP %d: %s", proto.PathDevicesReport, resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	err = json.Unmarshal(raw, &out)
	return out, err
}
