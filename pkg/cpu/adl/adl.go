// SPDX-License-Identifier: GPL-2.0-only

// Package adl describes the JTAG chain of an Alder Lake-S CPU (root TAP
// only).
//
// docs/models.md describes the TAP network.
package adl

import "libredci/pkg/model"

func cpu() *model.TAP {
	return &model.TAP{
		Name: "cpu", IRLen: 12, IDCode: 0x000f6113, IDCodeIR: 0x02,
		Registers: []*model.Register{
			{Name: "idcode", IR: 0x02, Bits: 32, Access: model.ReadOnly},
		},
	}
}

// Chip returns a fresh description of the CPU's chain.
func Chip() model.Chip {
	return model.Chip{Root: cpu()}
}
