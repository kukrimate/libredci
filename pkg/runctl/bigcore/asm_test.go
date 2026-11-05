// SPDX-License-Identifier: GPL-2.0-only
package bigcore

import (
	"bytes"
	"testing"
)

// Encodings as found in the reference tool's probe-mode traffic.
func TestEncodings(t *testing.T) {
	e16, e32 := enc{16}, enc{32}
	for _, c := range []struct {
		got, want []byte
	}{
		{e16.movR8Imm(rCL, 0), []byte{0xb1, 0x00}},
		{e16.movChCl(), []byte{0x88, 0xcd}},
		{e16.bswapECX32(), []byte{0x66, 0x0f, 0xc9}},
		{e16.bswapCX16(), []byte{0x0f, 0xc9}},
		{e16.movR8Imm(rAL, 1), []byte{0xb0, 0x01}},
		{e16.movR8Imm(rAH, 0), []byte{0xb4, 0x00}},
		{e16.rdmsr(), []byte{0x0f, 0x32}},
		{e32.movR32Imm(rCX, 0x1df), []byte{0xb9, 0xdf, 0x01, 0x00, 0x00}},
		{e32.andEAXImm(0xffffffef), []byte{0x25, 0xef, 0xff, 0xff, 0xff}},
		{e32.shrImm(rAX, 4), []byte{0xc1, 0xe8, 0x04}},
		{e32.movToDR(6, rAX), []byte{0x0f, 0x23, 0xf0}},
		{e32.movR32Imm(rBX, 2), []byte{0xbb, 0x02, 0x00, 0x00, 0x00}},
		{enc{64}.movR64Imm(rDX, 0x1000), []byte{0x48, 0xba, 0x00, 0x10, 0, 0, 0, 0, 0, 0}},
		{enc{64}.movRR(rAX, r8), []byte{0x4c, 0x89, 0xc0}},
		{e16.movR32Imm(rCX, 0x71), []byte{0x66, 0xb9, 0x71, 0, 0, 0}},
	} {
		if !bytes.Equal(c.got, c.want) {
			t.Errorf("got % x, want % x", c.got, c.want)
		}
	}
}
