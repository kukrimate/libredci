// SPDX-License-Identifier: GPL-2.0-only
package tapnet

import (
	"fmt"
	"strings"
	"testing"

	"libredci/pkg/jtag"
	"libredci/pkg/model"
	"libredci/pkg/platforms/kblspt"
)

// recorder pretends to be a probe: every captured bit reads as 0.
type recorder struct{ chain int }

func (r *recorder) SelectChain(n int) error { r.chain = n; return nil }
func (r *recorder) Execute(cmds []jtag.TCK) ([]bool, error) {
	n := 0
	for _, c := range cmds {
		if c&jtag.Capture != 0 {
			n++
		}
	}
	return make([]bool, n), nil
}

func session(t *testing.T) (*Session, *[]string) {
	var log []string
	s := New(kblspt.Platform(), &recorder{})
	s.Plat.Walk(func(tap *model.TAP, _ int) { s.present[tap] = true })
	s.OnTransaction = func(chain int, scans []*jtag.Scan) {
		var ops []string
		for _, sc := range scans {
			kind := "DR"
			if sc.IR {
				kind = "IR"
			} else if sc.Exchange {
				kind = "XDR"
			}
			ops = append(ops, fmt.Sprintf("%s[%d]=%#x", kind, sc.Bits, sc.In))
		}
		log = append(log, strings.Join(ops, " "))
	}
	return s, &log
}

func expect(t *testing.T, got []string, want ...string) {
	t.Helper()
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("transactions differ\n got:\n  %s\nwant:\n  %s", strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}
}

// Same scans as the reference tool's capture of reading the CSME TAP IDCODE.
func TestSplicedIDCode(t *testing.T) {
	s, log := session(t)
	tap, _ := s.TAP("pch.left.csme_par.csme")
	if _, err := s.ReadIDCode(tap); err != nil {
		t.Fatal(err)
	}
	s.Release()
	expect(t, *log,
		"IR[8]=0xff DR[2]=0x0", // TDO hold release
		"IR[8]=0x11 DR[16]=0x4 IR[16]=0xff11 DR[15]=0x4 IR[24]=0xffff11 DR[16]=0x4 "+
			"IR[32]=0xffffff0c DR[35]=0x0 "+
			"IR[32]=0xffff11ff DR[17]=0x0 IR[24]=0xff11ff DR[16]=0x0 IR[16]=0x11ff DR[17]=0x0",
		"IR[8]=0xff DR[2]=0x3",
	)
}

// Same scans (modulo the wake-polling loop) as the capture of reading CBO0's IDCODE.
func TestVirtualIDCode(t *testing.T) {
	s, log := session(t)
	tap, _ := s.TAP("cpu.cbo0")
	// The recorder returns zeros, so the package-awake poll never succeeds;
	// only check the arbitration step and the access itself here.
	_, err := s.ReadIDCode(tap)
	if err == nil || !strings.Contains(err.Error(), "package did not wake") {
		t.Fatalf("expected wake failure with fake driver, got %v", err)
	}
	if (*log)[0] != "IR[9]=0x7f XDR[7]=0x2" {
		t.Errorf("arbitration: got %s", (*log)[0])
	}
	if (*log)[1] != "IR[9]=0x50 DR[18]=0x6" {
		t.Errorf("wake: got %s", (*log)[1])
	}
}

func TestVirtualLayout(t *testing.T) {
	s, log := session(t)
	// Skip preconditions to look at the raw composition.
	for _, n := range []string{kblspt.CpuTapArbitrated, kblspt.CpuCbosAwake, kblspt.CpuCoresAwake, kblspt.CpuPackageAwake} {
		s.acquired[n] = true
	}
	cbo, _ := s.TAP("cpu.cbo0")
	s.ReadIDCode(cbo)
	core, _ := s.TAP("cpu.core1")
	s.Read(core, core.MustRegister("tap_status"))
	expect(t, *log,
		"IR[9]=0x100 DR[8]=0x2 IR[9]=0x101 DR[40]=0x0",
		"IR[9]=0x114 DR[8]=0x60 IR[9]=0x115 DR[45]=0x0",
	)
}

// Presence comes from the uncore link-status "exists" bit, read like the
// reference tool does (scan zeros through it). The recorder reads 0: absent.
func TestPresence(t *testing.T) {
	s, log := session(t)
	cbo, _ := s.TAP("cpu.cbo2")
	delete(s.present, cbo)
	_, err := s.ReadIDCode(cbo)
	if err == nil || !strings.Contains(err.Error(), "not present") {
		t.Fatalf("want absence error, got %v", err)
	}
	expect(t, *log, "IR[9]=0x10b DR[31]=0x0")
	if !s.KnownAbsent("cpu.cbo2") || s.KnownAbsent("cpu.cbo1") {
		t.Error("KnownAbsent wrong")
	}
}

// The ISH core is spliced in together with its partition's retime TAP
// (select 0x401); the retime sits between it and TDO.
func TestSplicedWithIDCode(t *testing.T) {
	s, log := session(t)
	tap, _ := s.TAP("pch.lb.ish_par.ish")
	if _, err := s.ReadIDCode(tap); err != nil {
		t.Fatal(err)
	}
	s.Release()
	expect(t, *log,
		"IR[8]=0xff DR[2]=0x0",
		"IR[8]=0x11 DR[16]=0x10 IR[16]=0xff11 DR[15]=0x1 IR[24]=0xffff11 DR[14]=0x401 "+
			"IR[40]=0xffffff02ff DR[36]=0x0 "+
			"IR[40]=0xffff11ffff DR[16]=0x0 IR[24]=0xff11ff DR[16]=0x0 IR[16]=0x11ff DR[17]=0x0",
		"IR[8]=0xff DR[2]=0x3",
	)
}
