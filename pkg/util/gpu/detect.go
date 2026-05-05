/*
Copyright © 2020-2023 The k3d Author(s)

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in
all copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
THE SOFTWARE.
*/

// Package gpu detects the host's GPU vendor and maps it to the k3d
// device-passthrough flags that should be applied for that vendor.
//
// Detection signals (in priority order):
//
//	Native Linux: nvidia-smi → /dev/nvidia* → lspci -d <vendor>: → /sys/class/drm/renderD*/device/vendor
//	WSL2:         nvidia-smi → vulkaninfo --summary → glxinfo -B → /dev/dxg presence (vendor unknown)
//	macOS:        always "none" (Docker Desktop's Linux VM has no GPU passthrough)
//
// We deliberately avoid powershell.exe / WMI on WSL2: those only work against
// Windows-native binaries and are the slow, brittle path we want to replace.
package gpu

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// probeTimeout bounds each external detection tool so a hung nvidia-smi /
// vulkaninfo / lspci can't stall cluster creation indefinitely.
const probeTimeout = 5 * time.Second

// Vendor is the detected GPU vendor.
type Vendor string

const (
	VendorNone    Vendor = "none"
	VendorNVIDIA  Vendor = "nvidia"
	VendorAMD     Vendor = "amd"
	VendorIntel   Vendor = "intel"
	VendorUnknown Vendor = "unknown" // GPU passthrough is configured but vendor could not be identified (mainly WSL2 without detection tools)
)

// Mapping is the set of cluster-create options that should be applied for a
// given vendor to get sensible device passthrough working out of the box.
type Mapping struct {
	// GPURequest, if non-empty, is what `--gpus` would have been set to.
	GPURequest string
	// Devices is what would have been passed via `--device` flags.
	Devices []string
	// Notes is a human-readable explanation of what was applied; surfaced to the user.
	Notes string
}

// Detect performs vendor detection. Returns VendorNone when no GPU is found
// or detection isn't possible (e.g. on macOS).
func Detect() Vendor {
	if runtime.GOOS == "darwin" {
		return VendorNone
	}
	if isWSL2() {
		return detectWSL2()
	}
	return detectNative()
}

// MapVendor returns the device-passthrough config implied by a vendor.
// VendorUnknown and VendorNone return zero mappings — the caller should
// not modify the user's config in those cases.
//
// nvidiaCDIAvailable is consulted only for VendorNVIDIA: when true, the
// mapping uses the CDI device string (`nvidia.com/gpu=all`) instead of
// the legacy `--gpus all` hook. The caller decides this — typically by
// asking the container runtime whether it has discovered an
// `nvidia.com/gpu` CDI device — so this package stays runtime-agnostic.
func MapVendor(v Vendor, nvidiaCDIAvailable bool) Mapping {
	switch v {
	case VendorNVIDIA:
		return mapNVIDIA(nvidiaCDIAvailable)
	case VendorAMD:
		devices := []string{"/dev/dri:/dev/dri:rwm"}
		if _, err := os.Stat("/dev/kfd"); err == nil {
			devices = append([]string{"/dev/kfd"}, devices...)
		}
		return Mapping{
			Devices: devices,
			Notes:   "AMD GPU detected — applied --device for /dev/dri (and /dev/kfd if present, for ROCm compute).",
		}
	case VendorIntel:
		return Mapping{
			Devices: []string{"/dev/dri:/dev/dri:rwm"},
			Notes:   "Intel GPU detected — applied --device for /dev/dri.",
		}
	}
	return Mapping{}
}

// mapNVIDIA picks between CDI and the legacy NVIDIA Container Toolkit hook.
// Split out from MapVendor so both branches are unit-testable without
// touching the host. Pass true to model a host where Docker (25+) and a
// registered nvidia.com/gpu CDI spec are both present.
func mapNVIDIA(cdiAvailable bool) Mapping {
	if cdiAvailable {
		return Mapping{
			Devices: []string{"nvidia.com/gpu=all"},
			Notes:   "NVIDIA GPU detected with CDI support — applied --device nvidia.com/gpu=all (override with --gpus all to force the legacy hook).",
		}
	}
	return Mapping{
		GPURequest: "all",
		Notes:      "NVIDIA GPU detected — applied --gpus all (legacy NVIDIA Container Toolkit hook; for CDI, register a spec via `nvidia-ctk cdi generate` and rerun).",
	}
}

// ParseVendor maps the user-supplied flag value to a Vendor. The empty
// string and "none" both disable the feature; "auto" triggers Detect().
// Returns an error for unknown values.
func ParseVendor(s string) (Vendor, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "":
		return VendorNone, nil
	case "auto":
		return Detect(), nil
	case "nvidia":
		return VendorNVIDIA, nil
	case "amd":
		return VendorAMD, nil
	case "intel":
		return VendorIntel, nil
	case "none":
		return VendorNone, nil
	}
	return VendorNone, fmt.Errorf("invalid --auto-gpu value %q: expected one of auto, nvidia, amd, intel, none", s)
}

// isWSL2 reports whether the current process is running inside WSL2.
func isWSL2() bool {
	if v, err := os.ReadFile("/proc/version"); err == nil {
		if strings.Contains(strings.ToLower(string(v)), "microsoft") {
			return true
		}
	}
	return os.Getenv("WSL_INTEROP") != "" || os.Getenv("WSL_DISTRO_NAME") != ""
}

// detectWSL2 chains nvidia-smi → vulkaninfo → glxinfo → /dev/dxg.
func detectWSL2() Vendor {
	if cmdSucceeds("nvidia-smi") {
		return VendorNVIDIA
	}
	if v := vendorFromTool("vulkaninfo", []string{"--summary"}); v != VendorNone {
		return v
	}
	if v := vendorFromTool("glxinfo", []string{"-B"}); v != VendorNone {
		return v
	}
	if _, err := os.Stat("/dev/dxg"); err == nil {
		// Passthrough is configured but no detection tool is installed; surface
		// VendorUnknown so the caller can prompt the user instead of silently
		// doing nothing.
		return VendorUnknown
	}
	return VendorNone
}

// detectNative chains nvidia-smi → /dev/nvidia* → lspci → /sys/class/drm.
func detectNative() Vendor {
	if cmdSucceeds("nvidia-smi") {
		return VendorNVIDIA
	}
	for _, p := range []string{"/dev/nvidia0", "/dev/nvidiactl"} {
		if _, err := os.Stat(p); err == nil {
			return VendorNVIDIA
		}
	}
	if v := vendorFromLspci(); v != VendorNone {
		return v
	}
	if v := vendorFromDRM(); v != VendorNone {
		return v
	}
	return VendorNone
}

// cmdSucceeds returns true when the named binary is on PATH and exits 0.
func cmdSucceeds(name string, args ...string) bool {
	if _, err := exec.LookPath(name); err != nil {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()
	return exec.CommandContext(ctx, name, args...).Run() == nil
}

// vendorFromTool runs `name args...` and parses its output for vendor names.
// Used for vulkaninfo and glxinfo on WSL2.
func vendorFromTool(name string, args []string) Vendor {
	if _, err := exec.LookPath(name); err != nil {
		return VendorNone
	}
	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, name, args...).Output()
	if err != nil {
		return VendorNone
	}
	return vendorFromString(string(out))
}

// vendorFromString picks a vendor out of free-form text. Order matters:
// match "nvidia" before "amd"/"radeon" before "intel" because some tooling
// surfaces multiple drivers per device and we want the most specific
// (discrete GPUs before the ubiquitous Intel iGPU).
func vendorFromString(s string) Vendor {
	low := strings.ToLower(s)
	switch {
	case strings.Contains(low, "nvidia"):
		return VendorNVIDIA
	case strings.Contains(low, "amd") || strings.Contains(low, "radeon"):
		return VendorAMD
	case strings.Contains(low, "intel"):
		return VendorIntel
	}
	return VendorNone
}

// vendorFromLspci tries `lspci -d <vendor>:` for each known GPU vendor ID.
// Returns the first match.
func vendorFromLspci() Vendor {
	if _, err := exec.LookPath("lspci"); err != nil {
		return VendorNone
	}
	type query struct {
		id     string
		vendor Vendor
	}
	// NVIDIA first because if a host has a discrete NVIDIA GPU and an Intel
	// iGPU, we want the discrete one — same priority as the bash version.
	queries := []query{
		{"10de", VendorNVIDIA},
		{"1002", VendorAMD},
		{"8086", VendorIntel},
	}
	for _, q := range queries {
		ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
		out, err := exec.CommandContext(ctx, "lspci", "-d", q.id+":").Output()
		cancel()
		if err != nil {
			continue
		}
		if hasGPUClass(string(out)) {
			return q.vendor
		}
	}
	return VendorNone
}

// hasGPUClass reports whether any line of lspci output looks like a GPU
// (VGA controller, 3D controller, or Display controller — the last covers
// APUs like AMD Strix Halo that show up as PCI class 0380).
func hasGPUClass(s string) bool {
	low := strings.ToLower(s)
	return strings.Contains(low, "vga compatible controller") ||
		strings.Contains(low, "3d controller") ||
		strings.Contains(low, "display controller")
}

// vendorFromDRM walks /sys/class/drm/renderD*/device/vendor for the
// PCI vendor ID. Used as a final fallback when lspci isn't installed.
func vendorFromDRM() Vendor {
	matches, err := filepath.Glob("/sys/class/drm/renderD*/device/vendor")
	if err != nil {
		return VendorNone
	}
	for _, p := range matches {
		raw, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		switch strings.TrimSpace(string(raw)) {
		case "0x10de":
			return VendorNVIDIA
		case "0x1002":
			return VendorAMD
		case "0x8086":
			return VendorIntel
		}
	}
	return VendorNone
}
