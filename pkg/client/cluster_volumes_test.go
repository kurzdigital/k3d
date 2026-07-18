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
	"testing"

	"github.com/stretchr/testify/assert"

	k3d "github.com/k3d-io/k3d/v5/pkg/types"
)

func TestAdoptedAnonymousVolumes(t *testing.T) {
	anon1 := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	anon2 := "fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210"
	nodes := []*k3d.Node{
		{Name: "server-0", Volumes: []string{
			"/host/path:/data",              // bind mount
			"k3d-test-images:/k3d/images",   // named managed volume
			anon1 + ":/var/lib/rancher/k3s", // adopted anonymous volume
			anon1 + ":/var/lib/rancher/k3s", // duplicate (dedup expected)
			"UPPERCASEHEX0123456789ABCDEF0123456789ABCDEF0123456789ABCDEF0123:/x", // not lowercase hex
		}},
		{Name: "agent-0", Volumes: []string{
			anon2 + ":/var/lib/kubelet:rw", // adopted, with mode suffix
			anon1,                          // no destination -> not a mount spec
		}},
	}

	got := adoptedAnonymousVolumes(nodes)
	assert.Equal(t, []string{anon1, anon2}, got)
}

func TestIsAnonymousVolumeName(t *testing.T) {
	assert.True(t, isAnonymousVolumeName("0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"))
	assert.False(t, isAnonymousVolumeName("k3d-test-images"))
	assert.False(t, isAnonymousVolumeName(""))
	assert.False(t, isAnonymousVolumeName("0123456789abcdef"))                                                 // too short
	assert.False(t, isAnonymousVolumeName("g123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")) // non-hex char
}
