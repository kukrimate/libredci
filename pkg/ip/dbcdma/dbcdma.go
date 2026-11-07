// SPDX-License-Identifier: GPL-2.0-only

// Package dbcdma programs DMA through an xHCI debug capability's GP2
// registers. The registers take a payload address, a status address and a
// length; setting the length's valid bit starts the transfer. The payload of a read then arrives on the
// DbC's DMA interface (dci.Probe.DMARead), and the DbC writes a status
// dword to the status address.
package dbcdma

import (
	"fmt"

	"libredci/pkg/ip/iosfsb"
	"libredci/pkg/model"
)

const (
	gp2InPayloadLo   = 0x1c
	gp2InPayloadHi   = 0x20
	gp2InPayloadQual = 0x24
	gp2InLength      = 0x28
	gp2InStatusLo    = 0x2c
	gp2InStatusHi    = 0x30
	gp2InStatusQual  = 0x34

	gp2Valid = 1 << 31
	// MaxLen is the largest transfer the DbC accepts.
	MaxLen = 0x7fffffff

	magic = 0xdeadbeef
)

// PSFDestination encodes a PSF destination ID, the qualifier of a payload
// or status address: decode mode [21:20], PSF [19:16], port group [15],
// port [14:11].
func PSFDestination(mode, psf, group, port uint32) uint32 {
	return (mode&3)<<20 | (psf&0xf)<<16 | (group&1)<<15 | (port&0xf)<<11
}

// Engine is a DbC's GP2 DMA engine, programmed through a sideband bridge.
type Engine struct {
	Sideband *iosfsb.Bridge
	// Port, BAR and FID address the xHCI's GP2 registers on the sideband.
	Port, BAR, FID uint8
	// Memory is the PSF destination of host memory.
	Memory uint32
	// StatusAddr and StatusDest are where the DbC writes the status
	// dword of a transfer.
	StatusAddr uint64
	StatusDest uint32
}

func (e *Engine) reg(off uint64) iosfsb.Msg {
	return iosfsb.Msg{Port: e.Port, Opcode: iosfsb.MemWrite, BAR: e.BAR, FID: e.FID, Offset: off, Bits: 32}
}

// StartRead programs the DbC to send n bytes from physical address addr
// over the DMA interface. It first checks that GP2 register writes take
// effect.
func (e *Engine) StartRead(t model.Target, addr uint64, n uint32) error {
	if n == 0 || n > MaxLen {
		return fmt.Errorf("dma: length %#x out of range", n)
	}
	if err := e.Sideband.Write(t, e.reg(gp2InPayloadLo), magic); err != nil {
		return err
	}
	v, err := e.Sideband.Read(t, e.reg(gp2InPayloadLo))
	if err != nil {
		return err
	}
	if v != magic {
		return fmt.Errorf("dma: DbC GP2 register writes do not take effect (read %#x)", v)
	}
	for _, w := range []struct {
		off uint64
		val uint64
	}{
		{gp2InPayloadLo, addr & 0xffffffff},
		{gp2InPayloadHi, addr >> 32},
		{gp2InPayloadQual, uint64(e.Memory)},
		{gp2InStatusLo, e.StatusAddr & 0xffffffff},
		{gp2InStatusHi, e.StatusAddr >> 32},
		{gp2InStatusQual, uint64(e.StatusDest)},
		{gp2InLength, uint64(n) | gp2Valid},
	} {
		if err := e.Sideband.Write(t, e.reg(w.off), w.val); err != nil {
			return err
		}
	}
	return nil
}
