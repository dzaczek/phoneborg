package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/dzaczek/phoneborg/proto"
)

// Firmware checks (docs/DECISIONS.md ADR-030): what firmware each phone runs,
// how old its security patch is and, with -firmware-check, whether a newer
// official build or a LineageOS build exists for it. The local part needs no
// network; the online part sends device codenames to Google, LineageOS and
// GitHub, so it is off by default.

const (
	// FirmwareCheckInterval is how often the sources are fetched again.
	FirmwareCheckInterval = 24 * time.Hour
	firmwareTick          = 10 * time.Minute
	firmwareRetry         = time.Hour // new phones and failed sources wait at least this long
	firmwareStartDelay    = time.Minute
	firmwareFetchTimeout  = 60 * time.Second
	firmwareMaxBody       = 16 << 20

	lineageBuildsURL = "https://download.lineageos.org/api/v2/devices/%s/builds"
	pixelOTAURL      = "https://developers.google.com/android/ota?hl=en"
	xiaomiTrackerURL = "https://raw.githubusercontent.com/XiaomiFirmwareUpdater/miui-updates-tracker/master/data/latest.yml"
)

// Kinds of FirmwareUpdate.
const (
	FirmwareUpdateNewer       = "update"      // a newer build of what the phone runs
	FirmwareUpdateUpgrade     = "upgrade"     // a newer LineageOS version than the one it runs
	FirmwareUpdateAlternative = "alternative" // LineageOS for a phone on its stock firmware
)

// FirmwareOptions configures the firmware check.
type FirmwareOptions struct {
	Online bool // fetch the sources (-firmware-check)
	// URLs override the sources, for tests; "" = the real ones.
	LineageURL, PixelURL, XiaomiURL string
}

// FirmwareUpdate is one newer build a source offers for a phone.
type FirmwareUpdate struct {
	Source  string `json:"source"` // "google", "xiaomi" or "lineageos"
	Kind    string `json:"kind"`   // FirmwareUpdateNewer, ...Upgrade or ...Alternative
	Version string `json:"version"`
	Date    string `json:"date,omitempty"`
	URL     string `json:"url,omitempty"`
	Note    string `json:"note,omitempty"`
}

// NodeFirmware is one phone's firmware and what is newer.
type NodeFirmware struct {
	NodeID         string           `json:"node_id"`
	Brand          string           `json:"brand,omitempty"`
	Device         string           `json:"device,omitempty"`
	Current        string           `json:"current,omitempty"`
	SecurityPatch  string           `json:"security_patch,omitempty"`
	PatchAgeMonths int              `json:"patch_age_months"` // -1 = unknown
	Bootloader     string           `json:"bootloader,omitempty"`
	Latest         bool             `json:"latest"` // a vendor source knows the device and has nothing newer
	Updates        []FirmwareUpdate `json:"updates,omitempty"`
}

// FirmwareReport is GET /admin/firmware.
type FirmwareReport struct {
	Online    bool              `json:"online"` // -firmware-check is on
	CheckedAt time.Time         `json:"checked_at,omitzero"`
	Errors    map[string]string `json:"errors,omitempty"` // per source, from the last check
	Nodes     []NodeFirmware    `json:"nodes"`
}

type lineageBuild struct {
	Version, Date, URL string // Date is YYYY-MM-DD
}

type pixelBuild struct {
	Build, Month, URL string // Month as Google writes it, e.g. "Sep 2026"
}

type xiaomiBuild struct {
	Codename, Branch, Version, Date, Link, Android string
}

// firmwareData is what the sources returned; nil maps = never fetched.
type firmwareData struct {
	lineage map[string]*lineageBuild // codename -> latest build; nil value = none
	pixel   map[string][]pixelBuild  // codename -> builds in Google's order (oldest first)
	xiaomi  []xiaomiBuild
}

type firmware struct {
	opts   FirmwareOptions
	client *http.Client
	log    *slog.Logger

	mu        sync.Mutex
	data      firmwareData
	checkedAt time.Time
	errs      map[string]string
	running   bool
}

func newFirmware(opts FirmwareOptions, log *slog.Logger) *firmware {
	if opts.LineageURL == "" {
		opts.LineageURL = lineageBuildsURL
	}
	if opts.PixelURL == "" {
		opts.PixelURL = pixelOTAURL
	}
	if opts.XiaomiURL == "" {
		opts.XiaomiURL = xiaomiTrackerURL
	}
	return &firmware{opts: opts, client: &http.Client{Timeout: firmwareFetchTimeout}, log: log.With("component", "firmware")}
}

// run checks once a day, and sooner for devices no check has covered yet,
// until ctx ends. nodes returns the current phones.
func (f *firmware) run(ctx context.Context, nodes func() []proto.Node) {
	if !f.opts.Online {
		return
	}
	// Phones re-register within seconds of a controller start; wait for them.
	select {
	case <-ctx.Done():
		return
	case <-time.After(firmwareStartDelay):
	}
	t := time.NewTicker(firmwareTick)
	defer t.Stop()
	for {
		if f.due(nodes()) {
			f.check(ctx, nodes())
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// due reports whether a check is needed: none yet, the last one is a day
// old, or a phone appeared that it did not cover.
func (f *firmware) due(nodes []proto.Node) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.checkedAt.IsZero() || time.Since(f.checkedAt) >= FirmwareCheckInterval {
		return true
	}
	if time.Since(f.checkedAt) < firmwareRetry {
		return false
	}
	for _, n := range nodes {
		if d := strings.ToLower(n.Inventory.Device); d != "" {
			if _, ok := f.data.lineage[d]; !ok {
				return true
			}
		}
		if isXiaomi(n.Inventory.Brand) && f.data.xiaomi == nil || isPixel(n.Inventory.Brand) && f.data.pixel == nil {
			return true
		}
	}
	return false
}

var errFirmwareBusy = errors.New("a firmware check is already running")

// check fetches the sources the given phones need. Sources that fail keep
// their previous data.
func (f *firmware) check(ctx context.Context, nodes []proto.Node) error {
	f.mu.Lock()
	if f.running {
		f.mu.Unlock()
		return errFirmwareBusy
	}
	f.running = true
	f.mu.Unlock()
	defer func() { f.mu.Lock(); f.running = false; f.mu.Unlock() }()

	codenames := map[string]bool{}
	var wantPixel, wantXiaomi bool
	for _, n := range nodes {
		inv := n.Inventory
		if d := strings.ToLower(inv.Device); d != "" {
			codenames[d] = true
		}
		wantPixel = wantPixel || isPixel(inv.Brand)
		wantXiaomi = wantXiaomi || isXiaomi(inv.Brand)
	}
	errs := map[string]string{}
	lineage := map[string]*lineageBuild{}
	for d := range codenames {
		b, err := f.fetchLineage(ctx, d)
		if err != nil {
			errs["lineageos"] = err.Error()
			continue
		}
		lineage[d] = b
	}
	var pixel map[string][]pixelBuild
	if wantPixel {
		body, err := f.fetch(ctx, f.opts.PixelURL)
		if err == nil {
			pixel = parsePixelOTA(body)
		} else {
			errs["google"] = err.Error()
		}
	}
	var xiaomi []xiaomiBuild
	if wantXiaomi {
		body, err := f.fetch(ctx, f.opts.XiaomiURL)
		if err == nil {
			xiaomi = parseXiaomiTracker(body)
		} else {
			errs["xiaomi"] = err.Error()
		}
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if f.data.lineage == nil {
		f.data.lineage = map[string]*lineageBuild{}
	}
	for d, b := range lineage {
		f.data.lineage[d] = b
	}
	if pixel != nil {
		f.data.pixel = pixel
	}
	if xiaomi != nil {
		f.data.xiaomi = xiaomi
	}
	f.checkedAt, f.errs = time.Now(), errs
	f.log.Info("firmware checked", "devices", len(codenames), "pixel", wantPixel, "xiaomi", wantXiaomi, "errors", len(errs))
	return nil
}

func (f *firmware) fetch(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "PhoneBorg firmware check (+https://github.com/dzaczek/phoneborg)")
	req.Header.Set("Cookie", "devsite_wall_acks=nexus-ota-tos") // Google's OTA page shows its terms first
	resp, err := f.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, firmwareMaxBody))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusNotFound {
		return nil, errNotFound
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: HTTP %d", url, resp.StatusCode)
	}
	return body, nil
}

var errNotFound = errors.New("not found")

// fetchLineage returns the newest LineageOS build for a codename, nil if
// LineageOS does not support it.
func (f *firmware) fetchLineage(ctx context.Context, codename string) (*lineageBuild, error) {
	body, err := f.fetch(ctx, fmt.Sprintf(f.opts.LineageURL, codename))
	if errors.Is(err, errNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return parseLineageBuilds(body)
}

func parseLineageBuilds(body []byte) (*lineageBuild, error) {
	var builds []struct {
		Version string `json:"version"`
		Date    string `json:"date"`
		Files   []struct {
			URL string `json:"url"`
		} `json:"files"`
	}
	if err := json.Unmarshal(body, &builds); err != nil {
		return nil, fmt.Errorf("lineageos: %w", err)
	}
	var best *lineageBuild
	for _, b := range builds {
		if best == nil || b.Date > best.Date {
			best = &lineageBuild{Version: b.Version, Date: b.Date}
			if len(b.Files) > 0 {
				best.URL = b.Files[0].URL
			}
		}
	}
	return best, nil
}

var (
	pixelRow     = regexp.MustCompile(`(?s)<tr id="[^"]*">(.*?)</tr>`)
	pixelLink    = regexp.MustCompile(`href="(https://dl\.google\.com/[^"]*/([a-z0-9_]+)-ota-[^"]+\.zip)"`)
	pixelVersion = regexp.MustCompile(`\(([A-Z0-9][A-Z0-9.]+), ([A-Z][a-z]{2} \d{4})([^)]*)\)`)
)

// parsePixelOTA reads Google's full OTA image page: per device, the builds in
// page order (oldest first). Carrier- and region-specific builds (an extra
// note after the month) are skipped.
func parsePixelOTA(body []byte) map[string][]pixelBuild {
	out := map[string][]pixelBuild{}
	for _, row := range pixelRow.FindAllSubmatch(body, -1) {
		link := pixelLink.FindSubmatch(row[1])
		ver := pixelVersion.FindSubmatch(row[1])
		if link == nil || ver == nil || strings.TrimSpace(string(ver[3])) != "" {
			continue
		}
		d := string(link[2])
		out[d] = append(out[d], pixelBuild{Build: string(ver[1]), Month: string(ver[2]), URL: string(link[1])})
	}
	return out
}

// parseXiaomiTracker reads XiaomiFirmwareUpdater's latest.yml: a flat list of
// "- key: value" entries.
func parseXiaomiTracker(body []byte) []xiaomiBuild {
	var out []xiaomiBuild
	var cur *xiaomiBuild
	for _, line := range strings.Split(string(body), "\n") {
		item := strings.HasPrefix(line, "- ")
		if !item && !strings.HasPrefix(line, "  ") {
			continue
		}
		k, v, ok := strings.Cut(strings.TrimSpace(strings.TrimPrefix(line, "- ")), ":")
		if !ok {
			continue
		}
		if item {
			out = append(out, xiaomiBuild{})
			cur = &out[len(out)-1]
		}
		if cur == nil {
			continue
		}
		v = strings.Trim(strings.TrimSpace(v), `'"`)
		switch k {
		case "codename":
			cur.Codename = v
		case "branch":
			cur.Branch = v
		case "version":
			cur.Version = v
		case "date":
			cur.Date = v
		case "link":
			cur.Link = v
		case "android":
			cur.Android = v
		}
	}
	return out
}

func isPixel(brand string) bool { return strings.EqualFold(brand, "google") }

func isXiaomi(brand string) bool {
	switch strings.ToLower(brand) {
	case "xiaomi", "redmi", "poco":
		return true
	}
	return false
}

// report evaluates every phone against the data fetched so far.
func (f *firmware) report(nodes []proto.Node, now time.Time) FirmwareReport {
	f.mu.Lock()
	defer f.mu.Unlock()
	r := FirmwareReport{Online: f.opts.Online, CheckedAt: f.checkedAt, Nodes: []NodeFirmware{}}
	if len(f.errs) > 0 {
		r.Errors = map[string]string{}
		for k, v := range f.errs {
			r.Errors[k] = v
		}
	}
	for _, n := range nodes {
		r.Nodes = append(r.Nodes, evalFirmware(n.ID, n.Inventory, f.data, now))
	}
	sort.Slice(r.Nodes, func(i, j int) bool { return r.Nodes[i].NodeID < r.Nodes[j].NodeID })
	return r
}

// evalFirmware compares one phone's firmware with the fetched sources.
func evalFirmware(id string, inv proto.Inventory, data firmwareData, now time.Time) NodeFirmware {
	nf := NodeFirmware{NodeID: id, Brand: inv.Brand, Device: inv.Device, SecurityPatch: inv.SecurityPatch,
		PatchAgeMonths: patchAgeMonths(inv.SecurityPatch, now), Bootloader: inv.Bootloader}
	onLineage := inv.LineageVersion != ""
	switch {
	case onLineage:
		nf.Current = "LineageOS " + inv.LineageVersion
	case isXiaomi(inv.Brand) && inv.BuildIncremental != "":
		nf.Current = inv.BuildIncremental
	default:
		nf.Current = firstNonEmptyStr(inv.BuildDisplay, inv.BuildID)
	}
	if !onLineage && isPixel(inv.Brand) {
		evalPixel(&nf, inv, data.pixel[strings.ToLower(inv.Device)])
	}
	if !onLineage && isXiaomi(inv.Brand) {
		evalXiaomi(&nf, inv, data.xiaomi)
	}
	if b := data.lineage[strings.ToLower(inv.Device)]; b != nil {
		evalLineage(&nf, inv, *b)
	}
	return nf
}

func evalPixel(nf *NodeFirmware, inv proto.Inventory, builds []pixelBuild) {
	if len(builds) == 0 {
		return
	}
	latest := builds[len(builds)-1]
	if strings.EqualFold(latest.Build, inv.BuildID) {
		nf.Latest = true
		return
	}
	u := FirmwareUpdate{Source: "google", Kind: FirmwareUpdateNewer, Version: latest.Build, Date: latest.Month, URL: latest.URL}
	known := false
	for _, b := range builds {
		known = known || strings.EqualFold(b.Build, inv.BuildID)
	}
	if !known {
		u.Note = "the installed build is not in Google's list (a carrier, beta or newer build): compare before updating"
	}
	nf.Updates = append(nf.Updates, u)
}

// xiaomiVariant is the device-and-region part of a MIUI/HyperOS version:
// "V816.0.5.0.TKHMIXM" -> "KHMIXM" (device KH, region MI, global). The first
// letter (the Android version) is dropped, so a HyperOS build on a newer
// Android still matches.
func xiaomiVariant(version string) string {
	i := strings.LastIndexByte(version, '.')
	if i < 0 || len(version)-i-1 < 5 {
		return ""
	}
	return version[i+2:]
}

func evalXiaomi(nf *NodeFirmware, inv proto.Inventory, builds []xiaomiBuild) {
	variant, device := xiaomiVariant(inv.BuildIncremental), strings.ToLower(inv.Device)
	if variant == "" || device == "" {
		return
	}
	var latest *xiaomiBuild
	installed := ""
	for i := range builds {
		b := &builds[i]
		if b.Branch != "Stable" || xiaomiVariant(b.Version) != variant ||
			(b.Codename != device && !strings.HasPrefix(b.Codename, device+"_")) {
			continue
		}
		if b.Version == inv.BuildIncremental {
			installed = b.Date
		}
		if latest == nil || b.Date > latest.Date {
			latest = b
		}
	}
	if latest == nil {
		return
	}
	newer := latest.Version != inv.BuildIncremental &&
		(installed != "" && latest.Date > installed || installed == "" && latest.Date > inv.SecurityPatch)
	if !newer {
		nf.Latest = true
		return
	}
	nf.Updates = append(nf.Updates, FirmwareUpdate{Source: "xiaomi", Kind: FirmwareUpdateNewer, Version: latest.Version,
		Date: latest.Date, URL: latest.Link, Note: "from the community tracker XiaomiFirmwareUpdater"})
}

func evalLineage(nf *NodeFirmware, inv proto.Inventory, b lineageBuild) {
	u := FirmwareUpdate{Source: "lineageos", Version: b.Version, Date: b.Date, URL: b.URL}
	if inv.LineageVersion == "" {
		u.Kind = FirmwareUpdateAlternative
		u.Note = "an official LineageOS build exists; it needs an unlocked bootloader and flashing wipes the phone"
		if inv.Bootloader == "locked" {
			u.Note += " (this phone's bootloader is locked)"
		}
		nf.Updates = append(nf.Updates, u)
		return
	}
	// "22.2-20260919-NIGHTLY-dipper" -> version 22.2, date 20260919.
	parts := strings.Split(inv.LineageVersion, "-")
	ver, date := parts[0], ""
	if len(parts) > 1 {
		date = parts[1]
	}
	switch {
	case versionLess(ver, b.Version):
		u.Kind = FirmwareUpdateUpgrade
		u.Note = "a newer LineageOS version; read the upgrade notes for this device first"
	case ver == b.Version && date != "" && strings.ReplaceAll(b.Date, "-", "") > date:
		u.Kind = FirmwareUpdateNewer
	default:
		nf.Latest = true
		return
	}
	nf.Updates = append(nf.Updates, u)
}

// versionLess compares dotted numeric versions ("22.2" < "23.2").
func versionLess(a, b string) bool {
	as, bs := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < max(len(as), len(bs)); i++ {
		var x, y int
		if i < len(as) {
			fmt.Sscan(as[i], &x)
		}
		if i < len(bs) {
			fmt.Sscan(bs[i], &y)
		}
		if x != y {
			return x < y
		}
	}
	return false
}

// patchAgeMonths is the age of a YYYY-MM-DD security patch in whole months,
// -1 if unknown.
func patchAgeMonths(patch string, now time.Time) int {
	t, err := time.Parse("2006-01-02", patch)
	if err != nil {
		return -1
	}
	m := (now.Year()-t.Year())*12 + int(now.Month()) - int(t.Month())
	if now.Day() < t.Day() {
		m--
	}
	return max(m, 0)
}

func firstNonEmptyStr(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func (s *Server) registerFirmwareAdmin(add func(pattern, action string, fn http.HandlerFunc)) {
	add("GET /admin/firmware", "firmware_get", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, s.firmware.report(s.reg.Snapshot(), time.Now()))
	})
	add("POST /admin/firmware/check", "firmware_check", func(w http.ResponseWriter, r *http.Request) {
		if !s.firmware.opts.Online {
			httpError(w, http.StatusConflict, "the online firmware check is off; start the controller with -firmware-check")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
		defer cancel()
		err := s.firmware.check(ctx, s.reg.Snapshot())
		s.audit(r, "firmware_check", err)
		if errors.Is(err, errFirmwareBusy) {
			httpError(w, http.StatusConflict, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, s.firmware.report(s.reg.Snapshot(), time.Now()))
	})
}
