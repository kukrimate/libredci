// SPDX-License-Identifier: GPL-2.0-only

// Package vcu drives a CPU uncore's VCU (validation control unit), which
// runs commands posted to a mailbox on the uncore TAP. Through it the probe
// can read and write uncore-local CSRs and device MMIO without halting any
// core.
//
// Mailbox (IR 0xc9, 65 bits): bit 1 run/busy, 16:9 index, 32:17 opcode,
// 64:33 data. The reply is read from IR 0xcb (same layout): run/busy
// clears, opcode holds a completion code, data the result.
package vcu

import (
	"fmt"
	"math/big"

	"libredci/pkg/model"
)

func f(name string, hi, lo int) model.Field { return model.Field{Name: name, Lo: lo, Hi: hi} }
func bit(name string, b int) model.Field    { return model.Field{Name: name, Lo: b, Hi: b} }

func mailbox(name string, ir uint64, zeroScanRead bool, requires []string) *model.Register {
	return &model.Register{Name: name, IR: ir, Bits: 65, Access: model.ReadWrite, ZeroScanRead: zeroScanRead,
		Requires: requires,
		Fields:   []model.Field{bit("run_busy", 1), f("index", 16, 9), f("opcode", 32, 17), f("data", 64, 33)}}
}

// Registers returns the mailbox registers for the uncore TAP's
// description. requires lists the conditions the VCU's commands need.
func Registers(requires ...string) []*model.Register {
	return []*model.Register{
		mailbox("vcu_mailbox", 0xc9, false, requires),
		mailbox("vcu_mailbox_read", 0xcb, true, requires),
	}
}

// Unit is a VCU reached through the mailbox registers of TAP.
type Unit struct {
	TAP string
}

// VCU command opcodes and sequence IDs.
const (
	vcuOpenSequence  = 0x02
	vcuCloseSequence = 0x03
	vcuReadData      = 0x06
	vcuWriteData     = 0x07
	vcuReadLocalCSR  = 0x11
	vcuWriteLocalCSR = 0x12
	vcuReadMMIO      = 0x13
	vcuWriteMMIO     = 0x14

	seqReadLocalCSR  = 0x1
	seqWriteLocalCSR = 0x2
	seqReadMMIO      = 0x3
	seqWriteMMIO     = 0x4

	vcuSuccess = 0x40
)

var vcuStatus = map[uint64]string{0x80: "timeout", 0x82: "thread unavailable", 0x90: "illegal"}

func (u Unit) command(t model.Target, opcode, data uint32) (uint32, error) {
	c, err := t.TAP(u.TAP)
	if err != nil {
		return 0, err
	}
	r, rd := c.MustRegister("vcu_mailbox"), c.MustRegister("vcu_mailbox_read")
	v := r.MustField("run_busy").Set(new(big.Int), 1)
	v = r.MustField("opcode").Set(v, uint64(opcode))
	v = r.MustField("data").Set(v, uint64(data))
	var status uint64
	for try := 0; try < 8; try++ {
		if _, err := t.Exchange(c, r, v); err != nil {
			return 0, err
		}
		var reply *big.Int
		busy := true
		for i := 0; i < 5 && busy; i++ {
			if reply, err = t.Scan(c, rd, nil); err != nil {
				return 0, err
			}
			busy = rd.MustField("run_busy").Get(reply) != 0
		}
		if busy {
			return 0, fmt.Errorf("vcu: command %#x still busy", opcode)
		}
		status = rd.MustField("opcode").Get(reply)
		if status == vcuSuccess {
			return uint32(rd.MustField("data").Get(reply)), nil
		}
	}
	name := vcuStatus[status]
	if name == "" {
		name = fmt.Sprintf("%#x", status)
	}
	return 0, fmt.Errorf("vcu: command %#x failed: %s", opcode, name)
}

// vcuAddr packs a CSR/MMIO address. size is 1, 2, 4 or 8 bytes; the
// device field is 3 bits wide.
func vcuAddr(dev, fn, bar int, offset uint16, size int) (uint32, error) {
	lc, ok := map[int]uint32{1: 0, 2: 1, 4: 2, 8: 3}[size]
	if !ok || dev < 0 || dev > 7 || fn < 0 || fn > 7 || bar < 0 || bar > 3 {
		return 0, fmt.Errorf("vcu: bad address (dev %d fn %d bar %d size %d)", dev, fn, bar, size)
	}
	return uint32(offset)<<16 | lc<<14 | uint32(dev)<<11 | uint32(fn)<<8 | uint32(bar)<<2, nil
}

func (u Unit) sequence(t model.Target, seq, op uint32, addr uint32, write bool, size int, val uint64) (uint64, error) {
	if _, err := u.command(t, vcuOpenSequence, seq); err != nil {
		return 0, err
	}
	var out uint64
	err := func() error {
		if _, err := u.command(t, op, addr); err != nil {
			return err
		}
		if write {
			if _, err := u.command(t, vcuWriteData, uint32(val)); err != nil {
				return err
			}
			if size == 8 {
				_, err := u.command(t, vcuWriteData, uint32(val>>32))
				return err
			}
			return nil
		}
		lo, err := u.command(t, vcuReadData, 0)
		if err != nil {
			return err
		}
		out = uint64(lo)
		if size == 8 {
			hi, err := u.command(t, vcuReadData, 0)
			if err != nil {
				return err
			}
			out |= uint64(hi) << 32
		}
		return nil
	}()
	// Always close the sequence, even after an error.
	if _, cerr := u.command(t, vcuCloseSequence, 1); err == nil {
		err = cerr
	}
	return out, err
}

// ReadCSR reads an uncore-local CSR (PCI device/function/offset).
func (u Unit) ReadCSR(t model.Target, dev, fn int, offset uint16, size int) (uint64, error) {
	a, err := vcuAddr(dev, fn, 0, offset, size)
	if err != nil {
		return 0, err
	}
	return u.sequence(t, seqReadLocalCSR, vcuReadLocalCSR, a, false, size, 0)
}

// WriteCSR writes an uncore-local CSR.
func (u Unit) WriteCSR(t model.Target, dev, fn int, offset uint16, size int, v uint64) error {
	a, err := vcuAddr(dev, fn, 0, offset, size)
	if err != nil {
		return err
	}
	_, err = u.sequence(t, seqWriteLocalCSR, vcuWriteLocalCSR, a, true, size, v)
	return err
}

// ReadMMIO reads a register in a device's MMIO BAR.
func (u Unit) ReadMMIO(t model.Target, dev, fn, bar int, offset uint16, size int) (uint64, error) {
	a, err := vcuAddr(dev, fn, bar, offset, size)
	if err != nil {
		return 0, err
	}
	return u.sequence(t, seqReadMMIO, vcuReadMMIO, a, false, size, 0)
}

// WriteMMIO writes a register in a device's MMIO BAR.
func (u Unit) WriteMMIO(t model.Target, dev, fn, bar int, offset uint16, size int, v uint64) error {
	a, err := vcuAddr(dev, fn, bar, offset, size)
	if err != nil {
		return err
	}
	_, err = u.sequence(t, seqWriteMMIO, vcuWriteMMIO, a, true, size, v)
	return err
}
