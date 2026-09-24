package provisioner

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// slimCategory is one group of user-facing apps slim can disable to free RAM
// on a phone dedicated to the cluster. Package names span vendors (AOSP,
// LineageOS, common OEM names); add a vendor's package name to a category to
// cover it, no code changes needed elsewhere.
type slimCategory struct {
	Name      string
	Packages  []string
	Telephony bool // only disabled when slim is asked to include telephony
}

// SlimTargets is the allowlist slim disables from, grouped by category.
var SlimTargets = []slimCategory{
	{Name: "camera", Packages: []string{"org.lineageos.aperture", "com.android.camera2", "org.codeaurora.snapcam"}},
	{Name: "gallery", Packages: []string{"org.lineageos.glimpse", "com.android.gallery3d"}},
	{Name: "music", Packages: []string{"org.lineageos.twelve", "com.android.music"}},
	{Name: "browser", Packages: []string{"org.lineageos.jelly", "com.android.browser"}},
	{Name: "calendar", Packages: []string{"org.lineageos.etar", "com.android.calendar"}},
	{Name: "clock", Packages: []string{"com.android.deskclock"}},
	{Name: "recorder", Packages: []string{"org.lineageos.recorder"}},
	{Name: "messaging", Packages: []string{"com.android.messaging"}},
	{Name: "email", Packages: []string{"com.android.email"}},
	{Name: "telephony", Telephony: true, Packages: []string{"com.android.contacts", "com.android.dialer"}},
}

// SetupWizardPackages are force-stopped once setup is marked complete, and
// are never disabled.
var SetupWizardPackages = []string{"org.lineageos.setupwizard", "com.google.android.setupwizard"}

// neverDisableSubstrings guards SlimTargets against ever touching a core
// system package, even if a future vendor addition to the table is careless.
// Matched case-insensitively, substring.
var neverDisableSubstrings = []string{
	"systemui", "launcher", "settings", "telephony", "provider",
	"permissioncontroller", "packageinstaller", "shell", "networkstack",
	"bluetooth", "wifi", "nfc", "setupwizard", "inputmethod",
}

// protectedPackages are exact package names neverDisableSubstrings would
// otherwise miss (e.g. the telephony service itself).
var protectedPackages = map[string]bool{
	"com.android.phone": true,
}

// isProtected reports whether pkg must never be disabled, regardless of
// SlimTargets.
func isProtected(pkg string) bool {
	if protectedPackages[pkg] {
		return true
	}
	lower := strings.ToLower(pkg)
	for _, s := range neverDisableSubstrings {
		if strings.Contains(lower, s) {
			return true
		}
	}
	return false
}

// ParseEnabledPackages parses `cmd package list packages -e` output
// ("package:<name>" per line) into package names.
func ParseEnabledPackages(out string) []string {
	var pkgs []string
	for _, line := range strings.Split(out, "\n") {
		if name, ok := strings.CutPrefix(strings.TrimSpace(line), "package:"); ok && name != "" {
			pkgs = append(pkgs, name)
		}
	}
	return pkgs
}

// ComputeDisableSet returns the SlimTargets packages that are both installed
// (present in installedEnabled) and safe to disable, sorted for a stable
// order. telephony includes the telephony category (dialer/contacts).
func ComputeDisableSet(installedEnabled []string, telephony bool) []string {
	installed := map[string]bool{}
	for _, p := range installedEnabled {
		installed[p] = true
	}
	var out []string
	for _, cat := range SlimTargets {
		if cat.Telephony && !telephony {
			continue
		}
		for _, pkg := range cat.Packages {
			if installed[pkg] && !isProtected(pkg) {
				out = append(out, pkg)
			}
		}
	}
	sort.Strings(out)
	return out
}

// FindSetupWizard returns which SetupWizardPackages are present in
// installed.
func FindSetupWizard(installed []string) []string {
	set := map[string]bool{}
	for _, p := range installed {
		set[p] = true
	}
	var found []string
	for _, p := range SetupWizardPackages {
		if set[p] {
			found = append(found, p)
		}
	}
	return found
}

// SlimStatePath records, on the device, exactly which packages slim disabled
// so unslim can restore them.
const SlimStatePath = RemoteDir + "/slim.state"

// ParseSlimState parses the slim state file: one package per line, blank
// lines ignored.
func ParseSlimState(data string) []string {
	var pkgs []string
	for _, line := range strings.Split(data, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			pkgs = append(pkgs, line)
		}
	}
	return pkgs
}

// MergeSlimState unions previously recorded packages with newly disabled
// ones, deduplicated and sorted, so repeated slim runs stay idempotent.
func MergeSlimState(recorded, newlyDisabled []string) []string {
	set := map[string]bool{}
	var out []string
	for _, p := range recorded {
		if p != "" && !set[p] {
			set[p] = true
			out = append(out, p)
		}
	}
	for _, p := range newlyDisabled {
		if p != "" && !set[p] {
			set[p] = true
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out
}

// FormatSlimState serializes packages for the slim state file, one per line.
func FormatSlimState(pkgs []string) string {
	if len(pkgs) == 0 {
		return ""
	}
	return strings.Join(pkgs, "\n") + "\n"
}

// ParseMemAvailable parses the MemAvailable line of /proc/meminfo (reported
// in kB) into bytes.
func ParseMemAvailable(meminfo string) (uint64, bool) {
	for _, line := range strings.Split(meminfo, "\n") {
		f := strings.Fields(line)
		if len(f) >= 2 && strings.TrimSuffix(f[0], ":") == "MemAvailable" {
			v, err := strconv.ParseUint(f[1], 10, 64)
			if err != nil {
				return 0, false
			}
			if len(f) > 2 && f[2] == "kB" {
				v *= 1024
			}
			return v, true
		}
	}
	return 0, false
}

// readMemAvailable reads and parses MemAvailable on serial.
func (p *Provisioner) readMemAvailable(ctx context.Context, serial string) (uint64, error) {
	out, err := p.ADB.Shell(ctx, serial, "cat /proc/meminfo")
	if err != nil {
		return 0, err
	}
	v, ok := ParseMemAvailable(out)
	if !ok {
		return 0, fmt.Errorf("MemAvailable not found in /proc/meminfo")
	}
	return v, nil
}

// Slim disables a conservative allowlist of user-facing apps (see
// SlimTargets) to free RAM on a phone dedicated to the cluster. telephony
// also disables dialer/contacts. It never touches system UI, the launcher,
// settings, telephony services, input methods or providers (see
// neverDisableSubstrings). It finishes the setup wizard if one is still
// pending, without disabling it, and force-stops Settings (it restarts on
// demand). It is idempotent: repeated runs merge into the on-device state
// file, so Unslim always restores everything Slim has ever disabled.
func (p *Provisioner) Slim(ctx context.Context, serial string, telephony bool) error {
	log := p.Log.With("serial", serial)

	before, err := p.readMemAvailable(ctx, serial)
	if err != nil {
		log.Warn("read MemAvailable failed", "when", "before", "err", err)
	}

	listOut, err := p.ADB.Shell(ctx, serial, "cmd package list packages -e")
	if err != nil {
		return fmt.Errorf("device %s: list packages: %w", serial, err)
	}
	installed := ParseEnabledPackages(listOut)

	var disabled []string
	for _, pkg := range ComputeDisableSet(installed, telephony) {
		if _, err := p.ADB.Shell(ctx, serial, "pm disable-user --user 0 "+pkg); err != nil {
			log.Warn("disable package failed", "pkg", pkg, "err", err)
			continue
		}
		disabled = append(disabled, pkg)
	}

	stateOut, _ := p.ADB.Shell(ctx, serial, "cat "+SlimStatePath+" 2>/dev/null; true")
	merged := MergeSlimState(ParseSlimState(stateOut), disabled)
	if len(merged) > 0 {
		write := fmt.Sprintf("mkdir -p %s && printf '%%s\\n' %s > %s",
			RemoteDir, strings.Join(merged, " "), SlimStatePath)
		if _, err := p.ADB.Shell(ctx, serial, write); err != nil {
			return fmt.Errorf("device %s: write slim state: %w", serial, err)
		}
	}

	// Finish the setup wizard if it never completed, without disabling it.
	if _, err := p.ADB.Shell(ctx, serial, "settings put secure user_setup_complete 1"); err != nil {
		log.Warn("settings user_setup_complete failed", "err", err)
	}
	if _, err := p.ADB.Shell(ctx, serial, "settings put global device_provisioned 1"); err != nil {
		log.Warn("settings device_provisioned failed", "err", err)
	}
	for _, pkg := range FindSetupWizard(installed) {
		if _, err := p.ADB.Shell(ctx, serial, "am force-stop "+pkg); err != nil {
			log.Warn("force-stop setup wizard failed", "pkg", pkg, "err", err)
		}
	}

	if _, err := p.ADB.Shell(ctx, serial, "am force-stop com.android.settings"); err != nil {
		log.Warn("force-stop settings failed", "err", err)
	}

	time.Sleep(3 * time.Second)
	after, err := p.readMemAvailable(ctx, serial)
	if err != nil {
		log.Warn("read MemAvailable failed", "when", "after", "err", err)
	}

	var freedMiB int64
	if before > 0 && after > 0 {
		freedMiB = int64(after-before) / (1024 * 1024)
	}
	log.Info("slim done", "freed_mib", freedMiB,
		"mem_available_before_mib", before/(1024*1024), "mem_available_after_mib", after/(1024*1024),
		"disabled", disabled)
	return nil
}

// Unslim re-enables every package Slim has ever disabled on serial (as
// recorded in the on-device state file) and removes that file.
func (p *Provisioner) Unslim(ctx context.Context, serial string) error {
	log := p.Log.With("serial", serial)

	stateOut, err := p.ADB.Shell(ctx, serial, "cat "+SlimStatePath+" 2>/dev/null; true")
	if err != nil {
		return fmt.Errorf("device %s: read slim state: %w", serial, err)
	}
	pkgs := ParseSlimState(stateOut)
	for _, pkg := range pkgs {
		if _, err := p.ADB.Shell(ctx, serial, "pm enable "+pkg); err != nil {
			log.Warn("enable package failed", "pkg", pkg, "err", err)
			continue
		}
	}
	if _, err := p.ADB.Shell(ctx, serial, "rm -f "+SlimStatePath); err != nil {
		log.Warn("remove slim state failed", "err", err)
	}
	log.Info("unslim done", "restored", pkgs)
	return nil
}
