// SPDX-License-Identifier: GPL-2.0-only

// Package kbl describes the JTAG chain of a Kaby Lake U/Y (incl. KBL-R)
// CPU.
//
// docs/models.md describes the TAP network and preconditions.
package kbl

import (
	"fmt"
	"math/big"
	"slices"
	"time"

	"libredci/pkg/ip/vcu"
	"libredci/pkg/model"
)

// Condition names.
const (
	TapArbitrated = "cpu_tap_arbitrated"
	PackageAwake  = "cpu_package_awake"
	CoresAwake    = "cpu_cores_awake"
	CbosAwake     = "cpu_cbos_awake"
)

// maxCores is how many cores/CBOs the die can have; which exist is read
// from the uncore's link-status registers at run time.
const maxCores = 4

// VCU is the uncore's VCU. Its commands need the cores awake: with only
// the package awake they time out, and with the package asleep the
// mailbox reads 0.
var VCU = vcu.Unit{TAP: "cpu"}

func f(name string, hi, lo int) model.Field { return model.Field{Name: name, Lo: lo, Hi: hi} }
func bit(name string, b int) model.Field    { return model.Field{Name: name, Lo: b, Hi: b} }

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
)

func cpuCore(n int) *model.TAP {
	return &model.TAP{
		Name: fmt.Sprintf("cpu.core%d", n), IRLen: 8,
		Via:      model.Virtual{IRSelect: 0x110 + 4*uint64(n), DRSelect: 0x111 + 4*uint64(n), DRPadLo: 8},
		Requires: []string{TapArbitrated, CoresAwake},
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
		Requires: []string{TapArbitrated, CbosAwake},
		ExistsIn: &model.ExistsBit{TAP: "cpu", Register: fmt.Sprintf("cbo%d_link", n), Field: "exists", Valid: linkValid},
	}
}

func cpu() *model.TAP {
	t := &model.TAP{
		Name: "cpu", IRLen: 9, IDCode: 0xaa76d013, IDCodeIR: 0x02, Arbitration: TapArbitrated,
		Registers: slices.Concat([]*model.Register{
			{Name: "idcode", IR: 0x02, Bits: 32, Access: model.ReadOnly},
			{Name: "silicon_id", IR: 0x40, Bits: 16, Access: model.ReadOnly,
				Fields: []model.Field{f("repo_id", 7, 0), f("die_cfg", 15, 8)}},
			regWakeReq, regBclkCtrl, regTapHandshake,
		}, vcu.Registers(TapArbitrated, CoresAwake), []*model.Register{
			{Name: "probe_mode_req", IR: 0x53, Bits: 1, Access: model.WriteOnly},
			{Name: "sa_status", IR: 0x51, Bits: 94, Access: model.ReadOnly, Fields: []model.Field{
				bit("tap_lock", 68), bit("probemode_redirect_req", 75), bit("probemode_preq", 76),
				bit("probemode_tap_req", 77), bit("probemode_prdy", 78), bit("ring_pwrgood", 83)}},
		}),
	}
	for n := 0; n < maxCores; n++ {
		t.Registers = append(t.Registers,
			linkStatus(fmt.Sprintf("cbo%d_link", n), 0x103+4*uint64(n), CbosAwake),
			linkStatus(fmt.Sprintf("core%d_link", n), 0x113+4*uint64(n), CoresAwake))
		t.Children = append(t.Children, cpuCbo(n))
	}
	for n := 0; n < maxCores; n++ {
		t.Children = append(t.Children, cpuCore(n))
	}
	return t
}

func conditions() map[string]*model.Condition {
	return map[string]*model.Condition{
		// Ask the CPU uncore for probe ownership of its TAP network.
		TapArbitrated: {Name: TapArbitrated, Acquire: func(t model.Target) (func(model.Target) error, error) {
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
		PackageAwake: {Name: PackageAwake, Acquire: func(t model.Target) (func(model.Target) error, error) {
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

		CoresAwake: {Name: CoresAwake, Requires: []string{PackageAwake},
			Acquire: wakeReq(50*time.Millisecond, false)},
		CbosAwake: {Name: CbosAwake, Requires: []string{PackageAwake},
			Acquire: wakeReq(5*time.Millisecond, true)},
	}
}

// wakeReq programs pcode_wake_req to wake all cores and limit C-states.
// Release restores the previous values of the fields it set and leaves the
// other fields (probe-mode flow and PREQ enables) as they are.
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
		set := map[string]uint64{"core_wakeup": mask, "core_cstate_limit": 1, "pkg_cstate_limit": 0}
		if ring {
			set["turbo_disable"], set["freeze_ia_freq"], set["sagv_disable"] = 1, 1, 1
		}
		old, err := t.Modify(c, regWakeReq, func(v *big.Int) *big.Int {
			for name, val := range set {
				v = regWakeReq.MustField(name).Set(v, val)
			}
			return v
		})
		if err != nil {
			return nil, err
		}
		t.Wait(settle)
		return func(t model.Target) error {
			_, err := t.Modify(c, regWakeReq, func(v *big.Int) *big.Int {
				for name := range set {
					f := regWakeReq.MustField(name)
					v = f.Set(v, f.Get(old))
				}
				return v
			})
			return err
		}, nil
	}
}

// Chip returns a fresh description of the CPU's chain.
func Chip() model.Chip {
	return model.Chip{
		Root:       cpu(),
		Conditions: conditions(),
		Transient:  []string{CoresAwake, CbosAwake, PackageAwake, TapArbitrated},
	}
}
