// SPDX-License-Identifier: GPL-2.0-only
package kblspt

import (
	"fmt"
	"math/big"

	"libredci/pkg/model"
)

// The uncore's VCU (validation control unit) runs commands posted to a
// mailbox on the uncore TAP. Through it the probe can read and write
// uncore-local CSRs and device MMIO without halting any core.
//
// Mailbox (IR 0xc9, 65 bits): bit 1 run/busy, 16:9 index, 32:17 opcode,
// 64:33 data. The reply is read from IR 0xcb (same layout): run/busy
// clears, opcode holds a completion code, data the result.

var (
	regVCUMailbox = &model.Register{Name: "vcu_mailbox", IR: 0xc9, Bits: 65, Access: model.ReadWrite,
		Requires: []string{CpuTapArbitrated},
		Fields:   []model.Field{bit("run_busy", 1), f("index", 16, 9), f("opcode", 32, 17), f("data", 64, 33)}}
	regVCUMailboxRead = &model.Register{Name: "vcu_mailbox_read", IR: 0xcb, Bits: 65, Access: model.ReadWrite,
		ZeroScanRead: true, Requires: []string{CpuTapArbitrated},
		Fields: []model.Field{bit("run_busy", 1), f("index", 16, 9), f("opcode", 32, 17), f("data", 64, 33)}}
)

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

func vcuCommand(t model.Target, opcode, data uint32) (uint32, error) {
	c, err := t.TAP("cpu")
	if err != nil {
		return 0, err
	}
	r := regVCUMailbox
	v := r.MustField("run_busy").Set(new(big.Int), 1)
	v = r.MustField("opcode").Set(v, uint64(opcode))
	v = r.MustField("data").Set(v, uint64(data))
	var status uint64
	for try := 0; try < 8; try++ {
		if _, err := t.Exchange(c, regVCUMailbox, v); err != nil {
			return 0, err
		}
		var reply *big.Int
		busy := true
		for i := 0; i < 5 && busy; i++ {
			if reply, err = t.Scan(c, regVCUMailboxRead, nil); err != nil {
				return 0, err
			}
			busy = regVCUMailboxRead.MustField("run_busy").Get(reply) != 0
		}
		if busy {
			return 0, fmt.Errorf("vcu: command %#x still busy", opcode)
		}
		status = regVCUMailboxRead.MustField("opcode").Get(reply)
		if status == vcuSuccess {
			return uint32(regVCUMailboxRead.MustField("data").Get(reply)), nil
		}
	}
	name := vcuStatus[status]
	if name == "" {
		name = fmt.Sprintf("%#x", status)
	}
	return 0, fmt.Errorf("vcu: command %#x failed: %s", opcode, name)
}

// vcuAddr packs a CSR/MMIO address. size is 1, 2, 4 or 8 bytes.
func vcuAddr(dev, fn, bar int, offset uint16, size int) (uint32, error) {
	lc, ok := map[int]uint32{1: 0, 2: 1, 4: 2, 8: 3}[size]
	if !ok || dev < 0 || dev > 31 || fn < 0 || fn > 7 || bar < 0 || bar > 3 {
		return 0, fmt.Errorf("vcu: bad address (dev %d fn %d bar %d size %d)", dev, fn, bar, size)
	}
	return uint32(offset)<<16 | lc<<14 | uint32(dev)<<11 | uint32(fn)<<8 | uint32(bar)<<2, nil
}

func vcuSequence(t model.Target, seq, op uint32, addr uint32, write bool, size int, val uint64) (uint64, error) {
	if _, err := vcuCommand(t, vcuOpenSequence, seq); err != nil {
		return 0, err
	}
	var out uint64
	err := func() error {
		if _, err := vcuCommand(t, op, addr); err != nil {
			return err
		}
		if write {
			if _, err := vcuCommand(t, vcuWriteData, uint32(val)); err != nil {
				return err
			}
			if size == 8 {
				_, err := vcuCommand(t, vcuWriteData, uint32(val>>32))
				return err
			}
			return nil
		}
		lo, err := vcuCommand(t, vcuReadData, 0)
		if err != nil {
			return err
		}
		out = uint64(lo)
		if size == 8 {
			hi, err := vcuCommand(t, vcuReadData, 0)
			if err != nil {
				return err
			}
			out |= uint64(hi) << 32
		}
		return nil
	}()
	// Always close the sequence, even after an error.
	if _, cerr := vcuCommand(t, vcuCloseSequence, 1); err == nil {
		err = cerr
	}
	return out, err
}

// ReadCSR reads an uncore-local CSR (PCI device/function/offset).
func ReadCSR(t model.Target, dev, fn int, offset uint16, size int) (uint64, error) {
	a, err := vcuAddr(dev, fn, 0, offset, size)
	if err != nil {
		return 0, err
	}
	return vcuSequence(t, seqReadLocalCSR, vcuReadLocalCSR, a, false, size, 0)
}

// WriteCSR writes an uncore-local CSR.
func WriteCSR(t model.Target, dev, fn int, offset uint16, size int, v uint64) error {
	a, err := vcuAddr(dev, fn, 0, offset, size)
	if err != nil {
		return err
	}
	_, err = vcuSequence(t, seqWriteLocalCSR, vcuWriteLocalCSR, a, true, size, v)
	return err
}

// ReadMMIO reads a register in a device's MMIO BAR.
func ReadMMIO(t model.Target, dev, fn, bar int, offset uint16, size int) (uint64, error) {
	a, err := vcuAddr(dev, fn, bar, offset, size)
	if err != nil {
		return 0, err
	}
	return vcuSequence(t, seqReadMMIO, vcuReadMMIO, a, false, size, 0)
}

// WriteMMIO writes a register in a device's MMIO BAR.
func WriteMMIO(t model.Target, dev, fn, bar int, offset uint16, size int, v uint64) error {
	a, err := vcuAddr(dev, fn, bar, offset, size)
	if err != nil {
		return err
	}
	_, err = vcuSequence(t, seqWriteMMIO, vcuWriteMMIO, a, true, size, v)
	return err
}
