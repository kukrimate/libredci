// SPDX-License-Identifier: GPL-2.0-only

// Package adp describes the JTAG chain of an Alder Point-S PCH.
//
// docs/models.md describes the TAP network.
package adp

import (
	"strings"

	"libredci/pkg/model"
)

func f(name string, hi, lo int) model.Field { return model.Field{Name: name, Lo: lo, Hi: hi} }
func bit(name string, b int) model.Field    { return model.Field{Name: name, Lo: b, Hi: b} }

// open lists the PCH TAPs below the partitions that read their IDCODE on a
// production part without any unlock. The other TAPs at that level read
// IDCODE 0 there and are optional.
var open = map[string]bool{
	"pch.cl.bottom.npk_par.npk":     true,
	"pch.cl.left.master_vnn.tpsb":   true,
	"pch.cl.left.master.dfx_secagg": true,
}

func pch() *model.TAP {
	root := &model.TAP{
		Name: "pch", IRLen: 8, IDCode: 0x000e6113, IDCodeIR: 0x02,
		Registers: []*model.Register{
			{Name: "idcode", IR: 0x02, Bits: 32, Access: model.ReadOnly, Fields: []model.Field{
				bit("one", 0), f("manufacturer", 11, 1), f("part", 27, 12), f("version", 31, 28)}},
			{Name: "cltcr1", IR: 0xc1, Bits: 32, Access: model.ReadWrite, Fields: []model.Field{
				bit("pmodef", 0), bit("pmodetpe", 1), f("pmodes", 9, 2), bit("pmode_en", 10)}},
			{Name: "cltdbg", IR: 0xc4, Bits: 32, Access: model.ReadWrite, Fields: []model.Field{
				bit("frcon_vccstg", 0), bit("vccst_pwrgd_stat", 1), bit("pmc_cpu_c10_gateb_stat", 2),
				bit("dfx_cpu_c10_gateb_stat", 3), bit("cpu_cltap_avail", 4)}},
			{Name: "tapcr", IR: 0xcd, Bits: 32, Access: model.ReadWrite, Fields: []model.Field{
				bit("boot_halt_strap", 0), bit("dfx_aggregator_a0_debug_strap", 1),
				bit("dfx_aggregator_consent_strap", 2), bit("bssb_rdy_rst_b", 3),
				bit("itp_odt_disable_strap", 4), bit("rosc_byp_sts", 5), f("xtal_freq_sts", 7, 6),
				bit("xtal_mode_sts", 8), bit("xtal_sel1_sts", 9), bit("xtal_sel2_sts", 10),
				bit("bscanmodeovr", 11), bit("prc_en", 17), bit("prc_go", 18), bit("shad_prc_halt_ack", 19),
				bit("pmcrstbrk_sts", 20), bit("cnvi_bscan_en", 21), bit("cnvi_func_bscan_en", 22),
				bit("acro_en", 23), bit("pwell_power_status", 24), bit("dft_spi_config_done", 25),
				bit("jtag_exi_iomux_disable", 26), bit("fivrvnnpwrgd_ovr", 27), bit("pltrsrt_sts", 28),
				bit("csme_reset_sts", 29), bit("rst_and_isolation_en", 30), bit("dfxtestmode_sts", 31)}},
		},
	}
	taps := map[string]*model.TAP{"pch": root}
	for _, p := range pchTAPs {
		link := model.Spliced{SelectIR: p.selectIR, Width: p.width, Bit: p.bit}
		if !strings.HasSuffix(p.name, ".retime") {
			link.With = []string{p.parent + ".retime"}
		}
		t := &model.TAP{
			Name: p.name, IRLen: p.irLen, IDCode: p.idcode, IDCodeIR: p.idcodeIR,
			Optional: strings.Count(p.name, ".") >= 4 && !open[p.name], Via: link,
		}
		taps[p.name] = t
		parent := taps[p.parent]
		parent.Children = append(parent.Children, t)
	}
	return root
}

// Chip returns a fresh description of the PCH's chain.
func Chip() model.Chip {
	return model.Chip{Root: pch()}
}
