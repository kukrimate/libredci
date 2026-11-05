// SPDX-License-Identifier: GPL-2.0-only
package lakemont

import (
	"fmt"
	"strings"
	"testing"

	"libredci/pkg/jtag"
	"libredci/pkg/platforms/kblspt"
	"libredci/pkg/tapnet"
)

type recorder struct{}

func (recorder) SelectChain(int) error { return nil }
func (recorder) Execute(cmds []jtag.TCK) ([]bool, error) {
	n := 0
	for _, c := range cmds {
		if c&jtag.Capture != 0 {
			n++
		}
	}
	return make([]bool, n), nil
}

// A register read on the CSME core produces the same scans as the
// reference tool's capture (eip read while halted).
func TestReadRegisterScans(t *testing.T) {
	s := tapnet.New(kblspt.Platform(), recorder{})
	var last []string
	s.OnTransaction = func(chain int, scans []*jtag.Scan) {
		last = nil
		for _, sc := range scans {
			kind := "DR"
			if sc.IR {
				kind = "IR"
			}
			last = append(last, fmt.Sprintf("%s[%d]=%#x", kind, sc.Bits, sc.In))
		}
	}
	tap, _ := s.TAP("pch.left.csme_par.csme")
	c := &Core{s: s, tap: tap, name: "csme", halted: true, orig: map[string]uint32{}}
	if _, err := c.ReadRegister("eip"); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(last, " ")
	want := "IR[32]=0xffffff06 DR[67]=0xc01d660000 IR[32]=0xffffff03 " +
		"IR[32]=0xffffff06 DR[67]=0xe9d660000 IR[32]=0xffffff03 " +
		"IR[32]=0xffffff06 DR[67]=0x4cf0000000000000 IR[32]=0xffffff03 " +
		"IR[32]=0xffffff08 DR[35]=0x0"
	if !strings.Contains(got, want) {
		t.Errorf("scans:\n%s\nwant to contain:\n%s", got, want)
	}
}

func TestSplit(t *testing.T) {
	got := fmt.Sprint(split(0x1001, 9))
	if got != "[[0 1] [1 2] [3 4] [7 2]]" {
		t.Errorf("split = %s", got)
	}
}

// The x86 -> probe instruction encoding reproduces the known constants.
func TestPIR(t *testing.T) {
	for _, c := range []struct {
		code []byte
		want uint64
	}{
		{[]byte{0x0f, 0x09}, pirWBINVD},
		{[]byte{0x8b, 0x10}, pirMemRd32},
		{[]byte{0x89, 0x10}, pirMemWr32},
		{[]byte{0x66, 0x8b, 0x10}, pirMemRd16},
		{[]byte{0xec}, pirIORd8},
		{[]byte{0xef}, pirIOWr32},
	} {
		if got := PIR(c.code...); got != c.want {
			t.Errorf("PIR(% x) = %#x, want %#x", c.code, got, c.want)
		}
	}
}
