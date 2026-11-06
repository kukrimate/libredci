// SPDX-License-Identifier: GPL-2.0-only
package kblspt

import "testing"

func TestVCUAddr(t *testing.T) {
	a, err := vcuAddr(2, 0, 0, 0x48, 4)
	if err != nil || a != 0x00489000 {
		t.Errorf("vcuAddr(2, 0, 0, 0x48, 4) = %#x, %v", a, err)
	}
	a, err = vcuAddr(7, 7, 3, 0xfffc, 1)
	if err != nil || a != 0xfffc3f0c {
		t.Errorf("vcuAddr(7, 7, 3, 0xfffc, 1) = %#x, %v", a, err)
	}
	for _, dev := range []int{-1, 8, 0x16} {
		if _, err := vcuAddr(dev, 0, 0, 0, 4); err == nil {
			t.Errorf("vcuAddr accepted device %#x", dev)
		}
	}
}
