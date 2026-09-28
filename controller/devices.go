package controller

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/dzaczek/phoneborg/controller/gateway"
	"github.com/dzaczek/phoneborg/proto"
)

// USB device auto-detection (docs/DECISIONS.md ADR-019): pcprov reports what
// it sees on adb (devices not yet nodes, provisioning in progress, failures)
// so the panel and pbctl show more than fully registered nodes, and so the
// operator can turn automatic provisioning off and drive it from here.

const deviceSettingsFileVersion = 1

// DeviceStaleAfter is how long without a report before a reporting host's
// devices are shown as stale (its pcprov may have stopped, or lost its own
// connection to adb); comfortably above the ~15s interval pcprov targets.
const DeviceStaleAfter = 45 * time.Second

// DeviceOptions configures USB device auto-detection.
type DeviceOptions struct {
	File string // persists auto_provision; "" = memory only
	// AutoProvision is the initial value (see LoadDeviceSettings); the
	// controller's default behaviour is true (auto-provision new devices).
	AutoProvision bool
}

type deviceSettingsFile struct {
	Version       int  `json:"version"`
	AutoProvision bool `json:"auto_provision"`
}

// LoadDeviceSettings reads the device settings file; a missing file means
// the default, auto_provision true (today's behaviour).
func LoadDeviceSettings(path string) (bool, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return true, nil
	}
	if err != nil {
		return true, err
	}
	var f deviceSettingsFile
	if err := json.Unmarshal(data, &f); err != nil {
		return true, fmt.Errorf("%s: %w (move it away to start with auto-provision on)", path, err)
	}
	if f.Version != deviceSettingsFileVersion {
		return true, fmt.Errorf("%s: unsupported version %d", path, f.Version)
	}
	return f.AutoProvision, nil
}

// DeviceView is one device as shown by the admin API: pcprov's report, the
// node it became (if any) and whether its reporting host has gone quiet.
type DeviceView struct {
	proto.DeviceStatus
	Host string `json:"host"`
	// NodeID and Alias link the device to a registered node, best-effort by
	// matching the node id against the serial: node-agent's node id is the
	// phone's ro.serialno (docs/DECISIONS.md ADR-001), which is normally the
	// same string adb reports. Empty when no such node is registered.
	NodeID string `json:"node_id,omitempty"`
	Alias  string `json:"alias,omitempty"`
	// Stale is true once this device's reporting host has not sent a report
	// for DeviceStaleAfter: pcprov may have stopped, or lost adb.
	Stale bool `json:"stale"`
	// Pending is true while an operator "provision now"/"retry" command is
	// queued for this serial, not yet acknowledged by pcprov.
	Pending bool `json:"pending"`
}

// Devices is the response of GET/PUT /admin/devices.
type Devices struct {
	AutoProvision bool         `json:"auto_provision"`
	Devices       []DeviceView `json:"devices"`
}

// hostDevices is one reporting pcprov's last device report.
type hostDevices struct {
	devices  map[string]proto.DeviceStatus // by serial
	lastSeen time.Time
}

// devices holds every reporting host's device view, the auto_provision
// setting and pending operator commands.
type devices struct {
	path string

	mu            sync.Mutex
	autoProvision bool
	byHost        map[string]*hostDevices
	pending       map[string]bool // serial -> operator command queued, not yet acked
}

func newDevices(path string, autoProvision bool) *devices {
	return &devices{path: path, autoProvision: autoProvision, byHost: map[string]*hostDevices{}, pending: map[string]bool{}}
}

// save must be called with mu held.
func (d *devices) save() error {
	if d.path == "" {
		return nil
	}
	data, err := json.MarshalIndent(deviceSettingsFile{Version: deviceSettingsFileVersion, AutoProvision: d.autoProvision}, "", " ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(d.path), 0o700); err != nil {
		return err
	}
	return gateway.WriteFileAtomic(d.path, data, 0o600)
}

// setAutoProvision changes the setting and persists it.
func (d *devices) setAutoProvision(on bool) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.autoProvision = on
	return d.save()
}

// report applies a pcprov report: replaces that host's device list, clears
// acknowledged commands and returns the settings pcprov must obey plus any
// commands still pending.
func (d *devices) report(rep proto.DeviceReport) proto.DeviceReportResponse {
	d.mu.Lock()
	defer d.mu.Unlock()
	host := d.byHost[rep.Host]
	if host == nil {
		host = &hostDevices{}
		d.byHost[rep.Host] = host
	}
	host.devices = make(map[string]proto.DeviceStatus, len(rep.Devices))
	for _, ds := range rep.Devices {
		host.devices[ds.Serial] = ds
	}
	host.lastSeen = time.Now()
	for _, s := range rep.Acked {
		delete(d.pending, s)
	}
	var cmds []proto.DeviceCommand
	for s := range d.pending {
		cmds = append(cmds, proto.DeviceCommand{Serial: s})
	}
	sort.Slice(cmds, func(i, j int) bool { return cmds[i].Serial < cmds[j].Serial })
	return proto.DeviceReportResponse{AutoProvision: d.autoProvision, Commands: cmds}
}

// requestProvision queues a one-shot "provision now"/"retry" command for
// serial, delivered in the next report response. It is idempotent: asking
// again before it is acknowledged changes nothing.
func (d *devices) requestProvision(serial string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.pending[serial] = true
}

// view returns every known device across every reporting host, and the
// current auto_provision setting.
func (d *devices) view(nodeBySerial map[string]proto.Node) Devices {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := Devices{AutoProvision: d.autoProvision, Devices: []DeviceView{}}
	now := time.Now()
	var hosts []string
	for h := range d.byHost {
		hosts = append(hosts, h)
	}
	sort.Strings(hosts)
	for _, h := range hosts {
		hd := d.byHost[h]
		stale := now.Sub(hd.lastSeen) >= DeviceStaleAfter
		var serials []string
		for s := range hd.devices {
			serials = append(serials, s)
		}
		sort.Strings(serials)
		for _, s := range serials {
			v := DeviceView{DeviceStatus: hd.devices[s], Host: h, Stale: stale, Pending: d.pending[s]}
			if n, ok := nodeBySerial[s]; ok {
				v.NodeID, v.Alias = n.ID, n.Alias
			}
			out.Devices = append(out.Devices, v)
		}
	}
	return out
}

// Admin handlers.

func (s *Server) registerDevicesAdmin(add func(pattern, action string, fn http.HandlerFunc)) {
	add("GET /admin/devices", "devices_list", s.adminDevices)
	add("PUT /admin/devices", "devices_set", s.adminSetDevices)
	add("POST /admin/devices/report", "devices_report", s.adminDevicesReport)
	add("POST /admin/devices/{serial}/provision", "devices_provision", s.adminDevicesProvision)
}

// nodesBySerial maps node id to node, for DeviceView's best-effort link.
func (s *Server) nodesBySerial() map[string]proto.Node {
	nodes, _ := s.reg.View()
	out := make(map[string]proto.Node, len(nodes))
	for _, n := range nodes {
		out[n.ID] = n
	}
	return out
}

func (s *Server) adminDevices(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.devices.view(s.nodesBySerial()))
}

func (s *Server) adminSetDevices(w http.ResponseWriter, r *http.Request) {
	var req struct {
		AutoProvision *bool `json:"auto_provision"`
	}
	if !decodeStrict(w, r, &req) {
		return
	}
	if req.AutoProvision == nil {
		httpError(w, http.StatusBadRequest, `body must be {"auto_provision": true|false}`)
		return
	}
	err := s.devices.setAutoProvision(*req.AutoProvision)
	s.audit(r, "devices_set", err, "auto_provision", *req.AutoProvision)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "cannot store device settings: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, s.devices.view(s.nodesBySerial()))
}

// adminDevicesReport is pcprov's report endpoint (ADR-019), authenticated
// like every other admin route. It is called every ~15s by every watching
// pcprov, so unlike a real operator action it is not logged with audit
// (which would spam the log); adminAuth's phoneborg_admin_actions_total
// still counts it.
func (s *Server) adminDevicesReport(w http.ResponseWriter, r *http.Request) {
	var rep proto.DeviceReport
	if !decodeStrict(w, r, &rep) {
		return
	}
	if rep.Host == "" {
		httpError(w, http.StatusBadRequest, "host is required")
		return
	}
	writeJSON(w, http.StatusOK, s.devices.report(rep))
}

func (s *Server) adminDevicesProvision(w http.ResponseWriter, r *http.Request) {
	serial := r.PathValue("serial")
	s.devices.requestProvision(serial)
	s.audit(r, "devices_provision", nil, "serial", serial)
	writeJSON(w, http.StatusAccepted, map[string]any{"serial": serial, "provision_requested": true})
}
