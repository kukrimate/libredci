// SPDX-License-Identifier: GPL-2.0-only

package platform

import "testing"

func TestIdentify(t *testing.T) {
	for _, c := range []struct {
		list   []*Chip
		idcode uint32
		want   string
	}{
		{PCHs, 0x9a506013, "spt"}, // SPT-LP C1
		{PCHs, 0x0a507013, ""},    // SPT-H
		{CPUs, 0xaa76d013, "kbl"},
		{CPUs, 0, ""},
	} {
		got := ""
		if chip := Identify(c.list, c.idcode); chip != nil {
			got = chip.Name
		}
		if got != c.want {
			t.Errorf("Identify(%#08x) = %q, want %q", c.idcode, got, c.want)
		}
	}
}

func TestNewPartial(t *testing.T) {
	cpu, _ := Find(CPUs, "kbl")
	p := New(nil, cpu)
	if p.Name != "kbl" || len(p.Chains) != 1 || p.Chains[0].Index != 1 || p.CSR == nil {
		t.Errorf("New(nil, kbl) = %s, chains %v, CSR %v", p.Name, p.Chains, p.CSR)
	}
	if _, ok := p.TAP("pch"); ok {
		t.Error("New(nil, kbl) has a pch TAP")
	}
}
