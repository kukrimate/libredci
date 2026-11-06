// SPDX-License-Identifier: GPL-2.0-only

// Package kblspt describes a Kaby Lake U/Y (incl. KBL-R) CPU paired with a
// Sunrise Point-LP PCH, as found in e.g. the ThinkPad T480, reached over
// DCI. Chain 0 is the PCH, chain 1 the CPU.
//
// docs/models.md describes the TAP networks and preconditions.
package kblspt

import (
	"fmt"
	"math/big"
	"time"

	"libredci/pkg/model"
)

// Precondition names.
const (
	PchTdoHoldReleased = "pch_tdo_hold_released"
	CpuTapArbitrated   = "cpu_tap_arbitrated"
	CpuPackageAwake    = "cpu_package_awake"
	CpuCoresAwake      = "cpu_cores_awake"
	CpuCbosAwake       = "cpu_cbos_awake"
)

// maxCores is how many cores/CBOs the die can have; which exist is read
// from the uncore's link-status registers at run time.
const maxCores = 4

func f(name string, hi, lo int) model.Field { return model.Field{Name: name, Lo: lo, Hi: hi} }
func bit(name string, b int) model.Field    { return model.Field{Name: name, Lo: b, Hi: b} }

// ---------------------------------------------------------------- PCH ----

// pchSub builds a PCH sub-TAP spliced in through its parent's select register.
func pchSub(name string, idcode uint32, width, bit int, optional bool, children ...*model.TAP) *model.TAP {
	return &model.TAP{
		Name: name, IRLen: 8, IDCode: idcode, IDCodeIR: 0x0c, Optional: optional,
		Via:      model.Spliced{SelectIR: 0x11, Width: width, Bit: bit},
		Requires: []string{PchTdoHoldReleased},
		Children: children,
	}
}

func withThreads(t *model.TAP, th ...model.Thread) *model.TAP {
	t.Threads = append(t.Threads, th...)
	return t
}

func pch() *model.TAP {
	return &model.TAP{
		Name: "pch", IRLen: 8, IDCode: 0x0a506013, IDCodeIR: 0x02,
		Registers: []*model.Register{
			{Name: "idcode", IR: 0x02, Bits: 32, Access: model.ReadOnly, Fields: []model.Field{
				bit("one", 0), f("manufacturer", 11, 1), f("part", 27, 12), f("version", 31, 28)}},
			{Name: "tdo_hold", IR: 0xff, Bits: 2, Access: model.WriteOnly},
			{Name: "cltcr1", IR: 0xc1, Bits: 32, Access: model.ReadWrite, Requires: []string{PchTdoHoldReleased},
				Fields: []model.Field{bit("pmodef", 0), bit("pmodetpe", 1), f("pmodes", 9, 2)}},
		},
		Children: []*model.TAP{
			pchSub("pch.master", 0x02080001, 16, 0, false,
				pchSub("pch.master.tpsb", 0x00082003, 24, 2, true),
				pchSub("pch.master.npk", 0x00082007, 24, 6, true),
				pchSub("pch.master.retime", 0x00082017, 24, 22, false),
			),
			pchSub("pch.left", 0x02080003, 16, 2, false,
				pchSub("pch.left.csme_par", 0x02086103, 14, 2, false,
					withThreads(pchSub("pch.left.csme_par.csme", 0x08086101, 14, 2, false),
						model.Thread{Name: "csme", Arch: "lakemont"}),
					pchSub("pch.left.csme_par.retime", 0x0008610b, 14, 12, false),
				),
			),
			pchSub("pch.lb", 0x02080005, 16, 4, false,
				pchSub("pch.lb.ish_par", 0x02088201, 14, 0, false,
					&model.TAP{
						Name: "pch.lb.ish_par.ish", IRLen: 8, IDCode: 0x28289013, IDCodeIR: 0x02, Optional: true,
						Via: model.Spliced{SelectIR: 0x11, Width: 12, Bit: 0,
							With: []string{"pch.lb.ish_par.retime"}},
						Requires: []string{PchTdoHoldReleased},
						Threads:  []model.Thread{{Name: "ish", Arch: "lakemont"}},
					},
					pchSub("pch.lb.ish_par.retime", 0x0008800b, 12, 10, false),
				),
			),
			pchSub("pch.bottom", 0x02080007, 16, 6, false),
			pchSub("pch.top", 0x02080009, 16, 8, false),
			pchSub("pch.agg", 0x0008000b, 16, 10, false),
			pchSub("pch.cltap_retime", 0x0008000f, 16, 14, false),
		},
	}
}

// ---------------------------------------------------------------- CPU ----

// Layout of the uncore's per-endpoint link status registers (cbo/core).
// They are read by scanning zeros (their writable fields are normally 0).
// CBO links are read with the CBOs awake, core links with the cores awake.
// After the wake request they read 0 (or, briefly, other values) until
// they match linkValid.
func linkStatus(name string, ir uint64, requires ...string) *model.Register {
	return &model.Register{Name: name, IR: ir, Bits: 31, Access: model.ReadWrite, ZeroScanRead: true,
		Requires: requires,
		Fields: []model.Field{bit("exists", 8), f("dynamic_ir", 17, 9), f("dynamic_dr", 26, 18),
			bit("fsm_error", 27), bit("access_without_power", 28), bit("power_good", 29), bit("disabled", 30)}}
}

var linkValid = map[string]uint64{"power_good": 1, "fsm_error": 0, "access_without_power": 0}

var (
	regTapHandshake = &model.Register{Name: "tap_handshake", IR: 0x7f, Bits: 7, Access: model.ReadWrite,
		Fields: []model.Field{bit("probe_owns", 0), bit("probe_request", 1), bit("vcu_owns", 2),
			bit("vcu_owns_edram", 3), bit("vcu_request", 4), f("vcu_end_type", 6, 5)}}
	regBclkCtrl = &model.Register{Name: "pcu_bclk_ctrl", IR: 0x50, Bits: 18, Access: model.ReadWrite,
		Fields: []model.Field{bit("c6c7_wake", 1), bit("c6c7_defer", 2), bit("ignore_wake", 3),
			bit("disable_jtag_gating", 4), f("boot_fsm_state", 13, 8)}}
	regWakeReq = &model.Register{Name: "pcode_wake_req", IR: 0x4f, Bits: 65, Access: model.ReadWrite,
		Fields: []model.Field{f("core_wakeup", 8, 1), f("core_cstate_limit", 35, 33), f("pkg_cstate_limit", 40, 37),
			f("core_preq_enable", 50, 43), bit("turbo_disable", 53), bit("freeze_ia_freq", 54),
			bit("probe_mode_pcode_flow", 55), bit("sagv_disable", 56)}}
	// Value pcode_wake_req held on the T480 when idle; used as the guess for
	// read-modify-write (it is corrected automatically if different).
	wakeReqTypical = new(big.Int).SetUint64(0x7f90e00000000)
)

func cpuCore(n int) *model.TAP {
	return &model.TAP{
		Name: fmt.Sprintf("cpu.core%d", n), IRLen: 8,
		Via:      model.Virtual{IRSelect: 0x110 + 4*uint64(n), DRSelect: 0x111 + 4*uint64(n), DRPadLo: 8},
		Requires: []string{CpuTapArbitrated, CpuCoresAwake},
		ExistsIn: &model.ExistsBit{TAP: "cpu", Register: fmt.Sprintf("core%d_link", n), Field: "exists", Valid: linkValid},
		Threads: []model.Thread{
			{Name: fmt.Sprintf("cpu.c%dt0", n), Arch: "kbl-core", Index: 0},
			{Name: fmt.Sprintf("cpu.c%dt1", n), Arch: "kbl-core", Index: 1},
		},
		Registers: []*model.Register{
			{Name: "tap_status", IR: 0x60, Bits: 37, Access: model.ReadOnly, Fields: []model.Field{
				bit("mnsclk_en", 1), bit("mclk_en", 2), bit("array_freeze", 3), bit("pm_req", 4),
				bit("pm_rdy_t1", 5), bit("pm_rdy_t0", 6), bit("pm_redirect_t1", 7), bit("pm_redirect_t0", 8),
				bit("alloc_stall_t1", 9), bit("alloc_stall_t0", 10), bit("ms_stall", 11), bit("mlc_stall", 12),
				bit("disabled_t1", 13), bit("disabled_t0", 14), bit("unlocked", 20),
				bit("lcp1_clk_en", 21), bit("lcp2_clk_en", 22)}},
			{Name: "probe_mode", IR: 0x86, Bits: 4, Access: model.WriteOnly, Fields: []model.Field{
				bit("entry_req", 1), bit("force_subpir_update", 2)}},
			{Name: "sub_pir_write", IR: 0x83, Bits: 134, Access: model.WriteOnly, Fields: []model.Field{
				f("data", 131, 4), bit("thread", 132)}},
			{Name: "pdr_t0", IR: 0x84, Bits: 66, Access: model.ReadOnly, Fields: []model.Field{f("data", 64, 1)}},
			{Name: "pdr_t1", IR: 0x85, Bits: 66, Access: model.ReadOnly, Fields: []model.Field{f("data", 64, 1)}},
		},
	}
}

func cpuCbo(n int) *model.TAP {
	return &model.TAP{
		Name: fmt.Sprintf("cpu.cbo%d", n), IRLen: 8, IDCode: 0x0a76b013, IDCodeIR: 0x02,
		Via:      model.Virtual{IRSelect: 0x100 + 4*uint64(n), DRSelect: 0x101 + 4*uint64(n), DRPadLo: 8},
		Requires: []string{CpuTapArbitrated, CpuCbosAwake},
		ExistsIn: &model.ExistsBit{TAP: "cpu", Register: fmt.Sprintf("cbo%d_link", n), Field: "exists", Valid: linkValid},
	}
}

func cpu() *model.TAP {
	t := &model.TAP{
		Name: "cpu", IRLen: 9, IDCode: 0xaa76d013, IDCodeIR: 0x02,
		Registers: []*model.Register{
			{Name: "idcode", IR: 0x02, Bits: 32, Access: model.ReadOnly},
			{Name: "silicon_id", IR: 0x40, Bits: 16, Access: model.ReadOnly,
				Fields: []model.Field{f("repo_id", 7, 0), f("die_cfg", 15, 8)}},
			regWakeReq, regBclkCtrl, regTapHandshake, regVCUMailbox, regVCUMailboxRead,
			{Name: "probe_mode_req", IR: 0x53, Bits: 1, Access: model.WriteOnly},
			{Name: "sa_status", IR: 0x51, Bits: 94, Access: model.ReadOnly, Fields: []model.Field{
				bit("tap_lock", 68), bit("probemode_redirect_req", 75), bit("probemode_preq", 76),
				bit("probemode_tap_req", 77), bit("probemode_prdy", 78), bit("ring_pwrgood", 83)}},
		},
	}
	for n := 0; n < maxCores; n++ {
		t.Registers = append(t.Registers,
			linkStatus(fmt.Sprintf("cbo%d_link", n), 0x103+4*uint64(n), CpuCbosAwake),
			linkStatus(fmt.Sprintf("core%d_link", n), 0x113+4*uint64(n), CpuCoresAwake))
		t.Children = append(t.Children, cpuCbo(n))
	}
	for n := 0; n < maxCores; n++ {
		t.Children = append(t.Children, cpuCore(n))
	}
	return t
}

// ------------------------------------------------------ preconditions ----

func conditions() map[string]*model.Condition {
	return map[string]*model.Condition{
		// The PCH holds TDO unless released; needed before using the
		// cluster-TAP select register.
		PchTdoHoldReleased: {Name: PchTdoHoldReleased, Acquire: func(t model.Target) (func(model.Target) error, error) {
			p, _ := t.TAP("pch")
			reg := p.MustRegister("tdo_hold")
			if _, err := t.Scan(p, reg, big.NewInt(0)); err != nil {
				return nil, err
			}
			return func(t model.Target) error { _, err := t.Scan(p, reg, big.NewInt(3)); return err }, nil
		}},

		// Ask the CPU uncore for probe ownership of its TAP network.
		CpuTapArbitrated: {Name: CpuTapArbitrated, Acquire: func(t model.Target) (func(model.Target) error, error) {
			c, _ := t.TAP("cpu")
			req := regTapHandshake.MustField("probe_request").Set(new(big.Int), 1)
			if _, err := t.Exchange(c, regTapHandshake, req); err != nil {
				return nil, err
			}
			return func(t model.Target) error {
				rel := regTapHandshake.MustField("probe_owns").Set(new(big.Int), 1)
				_, err := t.Exchange(c, regTapHandshake, rel)
				return err
			}, nil
		}},

		// Keep the package out of deep C-states (PCU may be fully off).
		CpuPackageAwake: {Name: CpuPackageAwake, Acquire: func(t model.Target) (func(model.Target) error, error) {
			c, _ := t.TAP("cpu")
			wake := big.NewInt(0x6) // c6c7_wake | c6c7_defer
			fWake, fDefer, fIgn := regBclkCtrl.MustField("c6c7_wake"), regBclkCtrl.MustField("c6c7_defer"), regBclkCtrl.MustField("ignore_wake")
			fFSM := regBclkCtrl.MustField("boot_fsm_state")
			var ok bool
			for i := 0; i < 500 && !ok; i++ {
				// Blind write: the register can't be read-modify-written while
				// the PCU is off. Capture shows the state before this write.
				v, err := t.Scan(c, regBclkCtrl, wake)
				if err != nil {
					return nil, err
				}
				fsm := fFSM.Get(v)
				ok = fWake.Get(v) == 1 && fDefer.Get(v) == 1 && fIgn.Get(v) == 0 && (fsm == 0x20 || fsm == 0x11)
				if !ok {
					t.Logf("waiting for package wake: bclk_ctrl=%#x", v)
					t.Wait(time.Millisecond)
				}
			}
			if !ok {
				return nil, fmt.Errorf("package did not wake (low-power state?)")
			}
			t.Wait(time.Millisecond)
			return func(t model.Target) error { _, err := t.Scan(c, regBclkCtrl, big.NewInt(0)); return err }, nil
		}},

		CpuCoresAwake: {Name: CpuCoresAwake, Requires: []string{CpuPackageAwake},
			Acquire: wakeReq(50*time.Millisecond, false)},
		CpuCbosAwake: {Name: CpuCbosAwake, Requires: []string{CpuPackageAwake},
			Acquire: wakeReq(5*time.Millisecond, true)},
	}
}

// wakeReq programs pcode_wake_req to wake all cores and limit C-states,
// and restores the previous value on release.
func wakeReq(settle time.Duration, ring bool) func(model.Target) (func(model.Target) error, error) {
	return func(t model.Target) (func(model.Target) error, error) {
		c, _ := t.TAP("cpu")
		// Wake every core not known to be absent (before discovery: all).
		var mask uint64
		for n := 0; n < maxCores; n++ {
			if !t.KnownAbsent(fmt.Sprintf("cpu.core%d", n)) {
				mask |= 1 << n
			}
		}
		old, err := t.Modify(c, regWakeReq, wakeReqTypical, func(v *big.Int) *big.Int {
			r := regWakeReq
			v = r.MustField("core_wakeup").Set(v, mask)
			v = r.MustField("core_cstate_limit").Set(v, 1)
			v = r.MustField("pkg_cstate_limit").Set(v, 0)
			if ring {
				v = r.MustField("turbo_disable").Set(v, 1)
				v = r.MustField("freeze_ia_freq").Set(v, 1)
				v = r.MustField("sagv_disable").Set(v, 1)
			}
			return v
		})
		if err != nil {
			return nil, err
		}
		t.Wait(settle)
		return func(t model.Target) error { _, err := t.Exchange(c, regWakeReq, old); return err }, nil
	}
}

// Platform returns a fresh description.
func Platform() *model.Platform {
	return (&model.Platform{
		Name:       "kbl-spt",
		Chains:     []model.Chain{{Index: 0, Root: pch()}, {Index: 1, Root: cpu()}},
		Conditions: conditions(),
	}).Init()
}
