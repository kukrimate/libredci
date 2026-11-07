// SPDX-License-Identifier: GPL-2.0-only

// Package spt describes the JTAG chain of a Sunrise Point-LP PCH.
//
// docs/models.md describes the TAP network and preconditions.
package spt

import (
	"math/big"

	"libredci/pkg/ip/iosfsb"
	"libredci/pkg/model"
)

// TPSB is the PCH's TAP-to-sideband bridge, a generation 2.0 TAP2IOSFSB.
// It operates only with the Trace Hub enabled (see docs/models.md).
var TPSB = iosfsb.Bridge{TAP: "pch.master.tpsb", Legacy: true}

// TdoHoldReleased is the condition releasing the PCH's TDO hold.
const TdoHoldReleased = "pch_tdo_hold_released"

func f(name string, hi, lo int) model.Field { return model.Field{Name: name, Lo: lo, Hi: hi} }
func bit(name string, b int) model.Field    { return model.Field{Name: name, Lo: b, Hi: b} }

// pchSub builds a PCH sub-TAP spliced in through its parent's select register.
func pchSub(name string, idcode uint32, width, bit int, optional bool, children ...*model.TAP) *model.TAP {
	return &model.TAP{
		Name: name, IRLen: 8, IDCode: idcode, IDCodeIR: 0x0c, Optional: optional,
		Via:      model.Spliced{SelectIR: 0x11, Width: width, Bit: bit},
		Requires: []string{TdoHoldReleased},
		Children: children,
	}
}

func withRegisters(t *model.TAP, r ...*model.Register) *model.TAP {
	t.Registers = append(t.Registers, r...)
	return t
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
			{Name: "cltcr1", IR: 0xc1, Bits: 32, Access: model.ReadWrite, Requires: []string{TdoHoldReleased},
				Fields: []model.Field{bit("pmodef", 0), bit("pmodetpe", 1), f("pmodes", 9, 2)}},
		},
		Children: []*model.TAP{
			pchSub("pch.master", 0x02080001, 16, 0, false,
				withRegisters(pchSub(TPSB.TAP, 0x00082003, 24, 2, true), iosfsb.Registers()...),
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
						Requires: []string{TdoHoldReleased},
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

// Chip returns a fresh description of the PCH's chain.
func Chip() model.Chip {
	return model.Chip{
		Root: pch(),
		Conditions: map[string]*model.Condition{
			// The PCH holds TDO unless released; needed before using the
			// cluster-TAP select register.
			TdoHoldReleased: {Name: TdoHoldReleased, Acquire: func(t model.Target) (func(model.Target) error, error) {
				p, _ := t.TAP("pch")
				reg := p.MustRegister("tdo_hold")
				if _, err := t.Scan(p, reg, big.NewInt(0)); err != nil {
					return nil, err
				}
				return func(t model.Target) error { _, err := t.Scan(p, reg, big.NewInt(3)); return err }, nil
			}},
		},
	}
}
