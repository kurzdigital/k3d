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
package client

import (
	"net/netip"
	"testing"

	k3d "github.com/k3d-io/k3d/v5/pkg/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFreeIP(t *testing.T) {
	gateway := netip.MustParseAddr("172.31.129.1")

	tests := []struct {
		name    string
		ipam    k3d.IPAM
		want    string
		wantErr bool
	}{
		{
			name: "no ip range: first free address in subnet",
			ipam: k3d.IPAM{
				IPPrefix: netip.MustParsePrefix("172.31.129.0/24"),
				IPsUsed:  []netip.Addr{gateway},
			},
			want: "172.31.129.2",
		},
		{
			name: "ip range set: dynamic range is skipped",
			ipam: k3d.IPAM{
				IPPrefix: netip.MustParsePrefix("172.31.129.0/24"),
				IPRange:  netip.MustParsePrefix("172.31.129.0/25"),
				IPsUsed:  []netip.Addr{gateway},
			},
			want: "172.31.129.128",
		},
		{
			name: "ip range set: used addresses outside the range are skipped",
			ipam: k3d.IPAM{
				IPPrefix: netip.MustParsePrefix("172.31.129.0/24"),
				IPRange:  netip.MustParsePrefix("172.31.129.128/25"),
				IPsUsed:  []netip.Addr{gateway, netip.MustParseAddr("172.31.129.2"), netip.MustParseAddr("172.31.129.3")},
			},
			want: "172.31.129.4",
		},
		{
			name: "ip range set: dynamic IPs in use never shift static allocation into the range",
			ipam: k3d.IPAM{
				IPPrefix: netip.MustParsePrefix("10.0.0.0/29"),
				IPRange:  netip.MustParsePrefix("10.0.0.4/30"),
				IPsUsed:  []netip.Addr{netip.MustParseAddr("10.0.0.1"), netip.MustParseAddr("10.0.0.2"), netip.MustParseAddr("10.0.0.3")},
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ip, err := freeIP(&k3d.ClusterNetwork{Name: "test", IPAM: tt.ipam})
			if tt.wantErr {
				assert.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, ip.String())
		})
	}
}
