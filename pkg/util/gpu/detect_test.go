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

package gpu

import (
	"strings"
	"testing"
)

func TestParseVendor(t *testing.T) {
	cases := []struct {
		in      string
		want    Vendor
		wantErr bool
	}{
		{"", VendorNone, false},
		{"auto", "", false}, // result depends on the host; we only check that it doesn't error
		{"AUTO", "", false},
		{"NVIDIA", VendorNVIDIA, false},
		{"  nvidia  ", VendorNVIDIA, false},
		{"amd", VendorAMD, false},
		{"intel", VendorIntel, false},
		{"none", VendorNone, false},
		{"radeon", VendorNone, true}, // not a valid flag value; user should pass "amd"
		{"garbage", VendorNone, true},
	}
	for _, c := range cases {
		got, err := ParseVendor(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("ParseVendor(%q): expected error, got nil (vendor=%q)", c.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseVendor(%q): unexpected error: %v", c.in, err)
			continue
		}
		// For "auto" the result is whatever the host happens to be — skip the
		// equality check, just make sure we got a known value.
		if strings.EqualFold(c.in, "auto") {
			switch got {
			case VendorNone, VendorNVIDIA, VendorAMD, VendorIntel, VendorUnknown:
			default:
				t.Errorf("ParseVendor(%q): got unknown vendor %q", c.in, got)
			}
			continue
		}
		if got != c.want {
			t.Errorf("ParseVendor(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestVendorFromString(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want Vendor
	}{
		{
			name: "vulkaninfo nvidia (native loader)",
			in:   "GPU0:\n  apiVersion = 1.3.250\n  deviceName = NVIDIA GeForce RTX 4090\n",
			want: VendorNVIDIA,
		},
		{
			name: "vulkaninfo amd via DZN bridge",
			in:   "GPU0:\n  deviceName = AMD Radeon RX 7900 XTX (DZN)\n",
			want: VendorAMD,
		},
		{
			name: "vulkaninfo intel via DZN bridge",
			in:   "GPU0:\n  deviceName = Intel(R) Arc(TM) A770 Graphics (DZN)\n",
			want: VendorIntel,
		},
		{
			name: "glxinfo amd via radeon kw",
			in:   "OpenGL renderer string: D3D12 (Radeon RX 6800)\n",
			want: VendorAMD,
		},
		{
			name: "glxinfo nvidia",
			in:   "OpenGL vendor string: NVIDIA Corporation\nOpenGL renderer string: NVIDIA RTX A4000/PCIe/SSE2\n",
			want: VendorNVIDIA,
		},
		{
			name: "no match",
			in:   "VirtIO software rasterizer",
			want: VendorNone,
		},
		{
			name: "empty",
			in:   "",
			want: VendorNone,
		},
		{
			name: "nvidia takes priority over intel mention (driver name in dump)",
			in:   "deviceName = NVIDIA RTX A4000\nimplicitLayerName = MESA-Intel-overlay\n",
			want: VendorNVIDIA,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := vendorFromString(c.in); got != c.want {
				t.Errorf("vendorFromString(...) = %q, want %q", got, c.want)
			}
		})
	}
}

func TestHasGPUClass(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"01:00.0 VGA compatible controller: NVIDIA Corporation ...", true},
		{"01:00.0 3D controller: NVIDIA Corporation ...", true},
		{"00:02.0 Display controller: AMD/ATI ... [Strix Halo]", true},
		{"01:00.0 Audio device: NVIDIA Corporation ...", false},
		{"", false},
	}
	for _, c := range cases {
		if got := hasGPUClass(c.in); got != c.want {
			t.Errorf("hasGPUClass(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestMapVendor(t *testing.T) {
	cases := []struct {
		v               Vendor
		wantHasDevices  bool // /dev/kfd presence on host is environment-dependent for AMD
		wantNonZeroNote bool
	}{
		// NVIDIA's GPURequest/Devices depend on host CDI availability —
		// covered deterministically in TestMapNVIDIA below.
		{VendorAMD, true, true},
		{VendorIntel, true, true},
		{VendorNone, false, false},
		{VendorUnknown, false, false},
	}
	for _, c := range cases {
		m := MapVendor(c.v, false)
		if (len(m.Devices) > 0) != c.wantHasDevices {
			t.Errorf("MapVendor(%q).Devices = %v, expected non-empty=%v", c.v, m.Devices, c.wantHasDevices)
		}
		if (m.Notes != "") != c.wantNonZeroNote {
			t.Errorf("MapVendor(%q).Notes empty? = %v, expected non-empty=%v", c.v, m.Notes == "", c.wantNonZeroNote)
		}
	}
}

func TestMapNVIDIA(t *testing.T) {
	t.Run("legacy hook when CDI unavailable", func(t *testing.T) {
		m := mapNVIDIA(false)
		if m.GPURequest != "all" {
			t.Errorf("GPURequest = %q, want %q", m.GPURequest, "all")
		}
		if len(m.Devices) != 0 {
			t.Errorf("Devices = %v, want empty", m.Devices)
		}
		if m.Notes == "" {
			t.Errorf("Notes empty, expected explanation")
		}
	})
	t.Run("CDI when available", func(t *testing.T) {
		m := mapNVIDIA(true)
		if m.GPURequest != "" {
			t.Errorf("GPURequest = %q, want empty (CDI mode does not use --gpus)", m.GPURequest)
		}
		want := []string{"nvidia.com/gpu=all"}
		if len(m.Devices) != 1 || m.Devices[0] != want[0] {
			t.Errorf("Devices = %v, want %v", m.Devices, want)
		}
		if m.Notes == "" {
			t.Errorf("Notes empty, expected explanation")
		}
	})
}
