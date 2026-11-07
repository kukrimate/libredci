// SPDX-License-Identifier: GPL-2.0-only

// Package platform lists the chips libredci can describe, identifies them
// on a target by their root TAP's IDCODE, and composes them into a
// platform: the PCH on chain 0 and the CPU on chain 1, with the access
// paths they offer beyond their TAPs.
package platform

import (
	"fmt"
	"math/big"
	"strings"

	"libredci/pkg/cpu/adl"
	"libredci/pkg/cpu/kbl"
	"libredci/pkg/jtag"
	"libredci/pkg/model"
	"libredci/pkg/pch/adp"
	"libredci/pkg/pch/spt"
	"libredci/pkg/tapnet"
)

// CSR reads and writes PCI configuration registers and device MMIO
// without halting a core.
type CSR interface {
	ReadCSR(t model.Target, dev, fn int, offset uint16, size int) (uint64, error)
	WriteCSR(t model.Target, dev, fn int, offset uint16, size int, v uint64) error
	ReadMMIO(t model.Target, dev, fn, bar int, offset uint16, size int) (uint64, error)
	WriteMMIO(t model.Target, dev, fn, bar int, offset uint16, size int, v uint64) error
}

// Chip is a chip libredci can describe. It is identified by its root TAP's
// IDCODE, ignoring the version field.
type Chip struct {
	Name, Desc string
	New        func() model.Chip
	// Access paths the chip offers; nil if it has none.
	CSR CSR
}

func (c *Chip) matches(idcode uint32) bool { return c.New().Root.IDCodeMatches(idcode) }

// PCHs are the known PCHs (chain 0), CPUs the known CPUs (chain 1).
var (
	PCHs = []*Chip{
		{Name: "spt", Desc: "Sunrise Point-LP", New: spt.Chip},
		{Name: "adp", Desc: "Alder Point-S", New: adp.Chip},
	}
	CPUs = []*Chip{
		{Name: "kbl", Desc: "Kaby Lake U/Y/R", New: kbl.Chip, CSR: kbl.VCU},
		{Name: "adl", Desc: "Alder Lake-S", New: adl.Chip},
	}
)

// Find returns the chip called name from list.
func Find(list []*Chip, name string) (*Chip, error) {
	var names []string
	for _, c := range list {
		if c.Name == name {
			return c, nil
		}
		names = append(names, c.Name)
	}
	return nil, fmt.Errorf("unknown chip %q (have %s)", name, strings.Join(names, ", "))
}

// Identify returns the chip in list whose root TAP has IDCODE idcode, or
// nil.
func Identify(list []*Chip, idcode uint32) *Chip {
	for _, c := range list {
		if c.matches(idcode) {
			return c
		}
	}
	return nil
}

// ReadRootIDCode reads the IDCODE of the TAP nearest TDO on chain: after
// Test-Logic-Reset every TAP selects its IDCODE (or BYPASS) register. A
// value with bit 0 clear means no IDCODE (BYPASS, or a chain that reads 0).
func ReadRootIDCode(d tapnet.Driver, chain int) (uint32, error) {
	if err := d.SelectChain(chain); err != nil {
		return 0, err
	}
	b := jtag.NewBuilder()
	b.Reset()
	sc := b.DR(32, new(big.Int))
	b.Idle(1)
	if err := b.Run(d); err != nil {
		return 0, err
	}
	return uint32(sc.Out.Uint64()), nil
}

// Detection is the result of Detect for one chain.
type Detection struct {
	Chain  int
	IDCode uint32
	Chip   *Chip // nil if the IDCODE is not a known chip
}

// Detect reads the root IDCODEs of chain 0 and chain 1 and identifies the
// PCH and the CPU.
func Detect(d tapnet.Driver) (pch, cpu Detection, err error) {
	pch.Chain, cpu.Chain = 0, 1
	if pch.IDCode, err = ReadRootIDCode(d, 0); err != nil {
		return
	}
	pch.Chip = Identify(PCHs, pch.IDCode)
	if cpu.IDCode, err = ReadRootIDCode(d, 1); err != nil {
		return
	}
	cpu.Chip = Identify(CPUs, cpu.IDCode)
	return
}

// Platform is a target description with its access paths. A nil access
// path means the platform does not offer it.
type Platform struct {
	*model.Platform
	PCH, CPU *Chip
	CSR      CSR
}

// New composes a platform from a PCH and a CPU; either may be nil.
func New(pch, cpu *Chip) *Platform {
	var chips [2]model.Chip
	var names []string
	p := &Platform{PCH: pch, CPU: cpu}
	for i, c := range []*Chip{pch, cpu} {
		if c == nil {
			continue
		}
		chips[i] = c.New()
		names = append(names, c.Name)
		if c.CSR != nil {
			p.CSR = c.CSR
		}
	}
	p.Platform = model.NewPlatform(strings.Join(names, "+"), chips[:]...)
	return p
}
