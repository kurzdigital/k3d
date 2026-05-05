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
	"testing"

	"github.com/docker/docker/api/types/system"
	"github.com/stretchr/testify/assert"
)

func TestHasNvidiaCDIDevice(t *testing.T) {
	cases := []struct {
		name    string
		devices []system.DeviceInfo
		want    bool
	}{
		{"nil", nil, false},
		{"empty", []system.DeviceInfo{}, false},
		{"exact all", []system.DeviceInfo{{ID: "nvidia.com/gpu=all"}}, true},
		{"exact index", []system.DeviceInfo{{ID: "nvidia.com/gpu=0"}}, true},
		// Must NOT match: the old prefix check accepted these false positives.
		{"prefix-only without =", []system.DeviceInfo{{ID: "nvidia.com/gpu"}}, false},
		{"sibling kind", []system.DeviceInfo{{ID: "nvidia.com/gpufoo=0"}}, false},
		{"other vendor", []system.DeviceInfo{{ID: "amd.com/gpu=all"}}, false},
		{"mixed list", []system.DeviceInfo{{ID: "amd.com/gpu=all"}, {ID: "nvidia.com/gpu=all"}}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, hasNvidiaCDIDevice(c.devices))
		})
	}
}

func TestHasNvidiaRuntime(t *testing.T) {
	cases := []struct {
		name           string
		runtimes       map[string]system.RuntimeWithStatus
		defaultRuntime string
		want           bool
	}{
		{"nil map, no default", nil, "", false},
		{"only runc", map[string]system.RuntimeWithStatus{"runc": {}}, "runc", false},
		{"nvidia registered", map[string]system.RuntimeWithStatus{"runc": {}, "nvidia": {}}, "runc", true},
		// default-runtime=nvidia counts even if the map is not populated
		{"nvidia as default only", nil, "nvidia", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, hasNvidiaRuntime(c.runtimes, c.defaultRuntime))
		})
	}
}
