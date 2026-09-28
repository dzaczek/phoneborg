package proto

import "time"

// USB device auto-detection (docs/DECISIONS.md ADR-019): pcprov's view of
// devices attached to adb, including ones that are not (yet, or no longer)
// registered nodes. Reported by "pcprov watch" to the controller's admin API
// so the panel and pbctl can see unauthorized/provisioning/failed devices,
// not only fully registered nodes.

// PathDevicesReport is the admin endpoint "pcprov watch" posts device
// reports to (see controller/devices.go).
const PathDevicesReport = "/admin/devices/report"

// Device statuses (DeviceStatus.Status). "new" is a ready adb device not yet
// provisioned this pcprov run; "waiting-authorization" covers every adb
// state other than "device" (unauthorized, offline, ...); "provisioning"
// while Provision runs, with Step naming the current step; "provisioned"
// after a successful attempt; "failed" after a failed one, with Error and an
// operator-facing Hint; "gone" is reported once after the device disappears,
// then dropped from the next report.
const (
	DeviceNew          = "new"
	DeviceWaitingAuth  = "waiting-authorization"
	DeviceProvisioning = "provisioning"
	DeviceProvisioned  = "provisioned"
	DeviceFailed       = "failed"
	DeviceGone         = "gone"
)

// DeviceStatus is pcprov's view of one USB device.
type DeviceStatus struct {
	Serial    string    `json:"serial"`
	ADBState  string    `json:"adb_state"`       // adb devices -l state: device, unauthorized, offline, ...
	Model     string    `json:"model,omitempty"` // adb devices -l "model:" field
	Status    string    `json:"status"`          // one of the Device* constants above
	Step      string    `json:"step,omitempty"`  // current provisioning step, Status == "provisioning"
	Error     string    `json:"error,omitempty"` // Status == "failed"
	Hint      string    `json:"hint,omitempty"`  // operator-facing suggestion
	FirstSeen time.Time `json:"first_seen"`
	LastSeen  time.Time `json:"last_seen"`
}

// DeviceReport is the body of POST /admin/devices/report: one pcprov
// instance's whole view of attached devices, replacing the controller's
// previous view from this host. Acked names the serials whose pending
// DeviceCommand (see DeviceReportResponse) pcprov executed this cycle
// (successfully or not), so the controller can stop resending it.
type DeviceReport struct {
	Host    string         `json:"host"` // identifies the reporting pcprov, e.g. its hostname
	Devices []DeviceStatus `json:"devices"`
	Acked   []string       `json:"acked,omitempty"`
}

// DeviceCommand is a one-shot operator request for one serial: attempt to
// provision it now, regardless of auto_provision or backoff. It covers both
// the panel/pbctl "Provision" (a device auto-provisioning left alone) and
// "Retry" (a failed one) actions, which are the same request to pcprov.
type DeviceCommand struct {
	Serial string `json:"serial"`
}

// DeviceReportResponse answers a device report with the controller-side
// settings pcprov must obey and any pending one-shot commands for its
// devices.
type DeviceReportResponse struct {
	AutoProvision bool            `json:"auto_provision"`
	Commands      []DeviceCommand `json:"commands,omitempty"`
}
