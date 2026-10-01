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
	"net/netip"
	"testing"

	"github.com/docker/docker/api/types/network"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDynamicIPRange(t *testing.T) {
	tests := []struct {
		subnet string
		want   string // empty: no range
	}{
		{"172.31.129.0/24", "172.31.129.128/25"},
		{"172.18.0.0/16", "172.18.128.0/17"},
		{"10.0.0.0/29", "10.0.0.4/30"},
		{"10.0.0.0/30", ""},
		{"10.0.0.0/32", ""},
		{"172.31.129.77/24", "172.31.129.128/25"}, // non-canonical prefix
	}

	for _, tt := range tests {
		t.Run(tt.subnet, func(t *testing.T) {
			got, ok := dynamicIPRange(netip.MustParsePrefix(tt.subnet))
			if tt.want == "" {
				assert.False(t, ok)
				return
			}
			require.True(t, ok)
			assert.Equal(t, tt.want, got.String())
		})
	}
}

func TestDynamicIPRangeKeepsGatewayOutside(t *testing.T) {
	subnet := netip.MustParsePrefix("172.31.129.0/24")
	ipRange, ok := dynamicIPRange(subnet)
	require.True(t, ok)
	assert.False(t, ipRange.Contains(subnet.Addr().Next()), "gateway must not be part of the dynamic range")
}

func TestParseIPAMIPRange(t *testing.T) {
	d := Docker{}

	ipam, err := d.parseIPAM(network.IPAMConfig{Subnet: "172.31.129.0/24", Gateway: "172.31.129.1", IPRange: "172.31.129.128/25"})
	require.NoError(t, err)
	assert.Equal(t, "172.31.129.128/25", ipam.IPRange.String())

	ipam, err = d.parseIPAM(network.IPAMConfig{Subnet: "172.31.129.0/24", Gateway: "172.31.129.1"})
	require.NoError(t, err)
	assert.False(t, ipam.IPRange.IsValid())

	_, err = d.parseIPAM(network.IPAMConfig{Subnet: "172.31.129.0/24", IPRange: "garbage"})
	assert.Error(t, err)
}
