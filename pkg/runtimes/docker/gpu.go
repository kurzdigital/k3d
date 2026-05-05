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

package docker

import (
	"strings"

	"github.com/docker/docker/api/types/system"
)

// nvidiaCDIKind is the CDI device-kind k3d cares about. A CDI device ID is a
// fully-qualified name of the form "<kind>=<name>" (e.g. "nvidia.com/gpu=all"),
// so we match on the kind segment with an explicit "=" boundary rather than a
// bare prefix — otherwise "nvidia.com/gpufoo" would falsely match.
const nvidiaCDIKind = "nvidia.com/gpu"

// hasNvidiaCDIDevice reports whether the given discovered-device list contains
// an `nvidia.com/gpu` CDI device. Split out from the daemon call so it can be
// unit-tested without a live engine.
func hasNvidiaCDIDevice(devices []system.DeviceInfo) bool {
	for _, d := range devices {
		if kind, _, ok := strings.Cut(d.ID, "="); ok && kind == nvidiaCDIKind {
			return true
		}
	}
	return false
}

// hasNvidiaRuntime reports whether the daemon has an "nvidia" runtime
// registered (NVIDIA Container Toolkit) — either as a named entry in the
// Runtimes map or as the daemon-wide default runtime. Split out from the
// daemon call so it can be unit-tested without a live engine.
func hasNvidiaRuntime(runtimes map[string]system.RuntimeWithStatus, defaultRuntime string) bool {
	if _, ok := runtimes["nvidia"]; ok {
		return true
	}
	return defaultRuntime == "nvidia"
}
