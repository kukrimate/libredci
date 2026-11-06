// SPDX-License-Identifier: GPL-2.0-only
package bigcore

import "testing"

func TestSplit(t *testing.T) {
	for _, c := range []struct {
		addr uint64
		n    int
		want [][2]int
	}{
		{0x1000, 16, [][2]int{{0, 4}, {4, 4}, {8, 4}, {12, 4}}},
		{0x1001, 7, [][2]int{{0, 1}, {1, 2}, {3, 4}}},
		{0x1002, 3, [][2]int{{0, 2}, {2, 1}}},
	} {
		got := split(c.addr, c.n)
		if len(got) != len(c.want) {
			t.Errorf("split(%#x, %d) = %v, want %v", c.addr, c.n, got, c.want)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("split(%#x, %d) = %v, want %v", c.addr, c.n, got, c.want)
				break
			}
		}
	}
}
