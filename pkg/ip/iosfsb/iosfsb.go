// SPDX-License-Identifier: GPL-2.0-only

// Package iosfsb accesses a PCH's IOSF sideband through its TAP2IOSFSB
// (TPSB) TAP. A message is described in sbmsggo; updating it sends the
// message. sbmsgrsp then holds the completion status, the response code
// and, for reads, the data.
package iosfsb

import (
	"fmt"
	"math/big"

	"libredci/pkg/model"
)

func f(name string, hi, lo int) model.Field { return model.Field{Name: name, Lo: lo, Hi: hi} }
func bit(name string, b int) model.Field    { return model.Field{Name: name, Lo: b, Hi: b} }

var (
	regSbMsgGo = &model.Register{Name: "sbmsggo", IR: 0x30, Bits: 96, Access: model.WriteOnly,
		Fields: []model.Field{f("status", 1, 0), bit("broadcasten", 2), bit("npwrite", 3), bit("extdatawr", 4),
			f("msgtrigsel", 7, 6), f("portid", 15, 8), f("opcode", 23, 16), f("bar", 29, 27), bit("addrlen", 30),
			bit("eh", 31), f("fbe", 35, 32), f("sbe", 39, 36), f("fid", 47, 40), f("addr", 63, 48), f("data", 95, 64)}}
	regSbMsgRsp = &model.Register{Name: "sbmsgrsp", IR: 0x31, Bits: 96, Access: model.ReadOnly,
		Fields: []model.Field{f("status", 1, 0), f("rsp", 28, 27), f("data", 95, 32)}}
	regSbExtAddr   = &model.Register{Name: "sbextaddr", IR: 0x33, Bits: 32, Access: model.WriteOnly}
	regSbRootSpace = &model.Register{Name: "sbrootspc", IR: 0x35, Bits: 5, Access: model.WriteOnly}
	regSbExtDataWr = &model.Register{Name: "sbextdatawr", IR: 0x36, Bits: 32, Access: model.WriteOnly}
)

// Registers returns the TPSB's registers, for a chip's TAP description.
func Registers() []*model.Register {
	return []*model.Register{regSbMsgGo, regSbMsgRsp, regSbExtAddr, regSbRootSpace, regSbExtDataWr}
}

// Sideband opcodes; the read opcode of each pair is even.
const (
	MemRead  = 0x0
	MemWrite = 0x1
	IORead   = 0x2
	IOWrite  = 0x3
	CfgRead  = 0x4
	CfgWrite = 0x5
	CRRead   = 0x6
	CRWrite  = 0x7
)

// Spaces maps address space names to their write opcodes.
var Spaces = map[string]uint8{"mem": MemWrite, "io": IOWrite, "cfg": CfgWrite, "cr": CRWrite}

// sbmsgrsp status and response codes.
const (
	statusComplete = 0x3
	polls          = 6
)

var statusText = map[uint64]string{0: "no transaction started", 1: "transaction started, not complete", 2: "transaction pending"}
var responseText = map[uint64]string{1: "unsupported request", 2: "endpoint powered down", 3: "multicast mixed status"}

// Bridge is a TPSB TAP.
type Bridge struct {
	TAP string
	// Legacy marks a generation 2.0 TPSB (Sunrise Point, Lewisburg, Cannon
	// Point): it has no extended data or second byte enables, so 64-bit
	// accesses are split into two 32-bit ones.
	Legacy bool
}

// Msg is one sideband register access.
type Msg struct {
	Port   uint8
	Opcode uint8 // write opcode of the pair; reads use opcode & ^1
	BAR    uint8
	FID    uint8 // PCI device << 3 | function
	Offset uint64
	Bits   int // 8, 16, 32 or 64
	// RootSpace sets the message's root space (with the extended
	// header); negative leaves it out.
	RootSpace int
	Posted    bool
}

func byteEnables(bits int, off uint64) (uint64, error) {
	switch {
	case bits == 64:
		return 0xff, nil
	case bits == 32:
		return 0xf, nil
	case bits == 16 && off&3 == 0, bits == 16 && off&3 == 2:
		return 0x3 << (off & 3), nil
	case bits == 8:
		return 0x1 << (off & 3), nil
	}
	return 0, fmt.Errorf("sideband: %d-bit access at offset %#x is not supported", bits, off)
}

// send writes the message registers; data is nil for reads.
func (b Bridge) send(t model.Target, m Msg, data *uint64) error {
	tap, err := t.TAP(b.TAP)
	if err != nil {
		return err
	}
	be, err := byteEnables(m.Bits, m.Offset)
	if err != nil {
		return err
	}
	opcode := uint64(m.Opcode)
	if data == nil {
		opcode &^= 1
	}
	ext := m.Bits == 64
	if ext && data != nil {
		if _, err := t.Scan(tap, regSbExtDataWr, new(big.Int).SetUint64(*data>>32)); err != nil {
			return err
		}
	}
	r := regSbMsgGo
	v := new(big.Int)
	set := func(name string, val uint64) { v = r.MustField(name).Set(v, val) }
	if !m.Posted {
		set("npwrite", 1)
	}
	if ext {
		set("extdatawr", 1)
	}
	set("sbe", be>>4&0xf)
	set("portid", uint64(m.Port))
	set("opcode", opcode)
	set("bar", uint64(m.BAR))
	set("fbe", be&0xf)
	set("fid", uint64(m.FID))
	set("addr", m.Offset&0xffff)
	rs := uint64(0)
	if m.RootSpace >= 0 {
		set("eh", 1)
		rs = uint64(m.RootSpace)
	}
	if _, err := t.Scan(tap, regSbRootSpace, new(big.Int).SetUint64(rs)); err != nil {
		return err
	}
	if m.Offset > 0xffff {
		if _, err := t.Scan(tap, regSbExtAddr, new(big.Int).SetUint64(m.Offset>>16)); err != nil {
			return err
		}
		set("addrlen", 1)
	}
	if data != nil {
		set("data", *data&0xffffffff)
	}
	_, err = t.Scan(tap, r, v)
	return err
}

// split returns the two 32-bit halves of a 64-bit access on a legacy
// bridge.
func (b Bridge) split(m Msg) (lo, hi Msg, ok bool) {
	if !b.Legacy || m.Bits != 64 {
		return m, m, false
	}
	lo, hi = m, m
	lo.Bits, hi.Bits = 32, 32
	hi.Offset += 4
	return lo, hi, true
}

func (b Bridge) read(t model.Target, m Msg) (uint64, error) {
	if err := b.send(t, m, nil); err != nil {
		return 0, err
	}
	tap, _ := t.TAP(b.TAP)
	r := regSbMsgRsp
	var v *big.Int
	var status uint64
	for i := 0; i < polls; i++ {
		var err error
		if v, err = t.Scan(tap, r, nil); err != nil {
			return 0, err
		}
		if status = r.MustField("status").Get(v); status == statusComplete {
			break
		}
	}
	if status != statusComplete {
		return 0, fmt.Errorf("sideband: %s, IOSF sideband may be hung", statusText[status])
	}
	if rsp := r.MustField("rsp").Get(v); rsp != 0 {
		return 0, fmt.Errorf("sideband: %s", responseText[rsp])
	}
	d := r.MustField("data").Get(v)
	if m.Bits < 32 {
		d = d >> (8 * (m.Offset & 3)) & (1<<m.Bits - 1)
	} else if m.Bits == 32 {
		d &= 0xffffffff
	}
	return d, nil
}

// Read reads a sideband register.
func (b Bridge) Read(t model.Target, m Msg) (uint64, error) {
	lo, hi, split := b.split(m)
	if !split {
		return b.read(t, m)
	}
	l, err := b.read(t, lo)
	if err != nil {
		return 0, err
	}
	h, err := b.read(t, hi)
	return h<<32 | l, err
}

// Write writes a sideband register. val is placed in the data dword as
// given, with the byte enables selecting the lanes written. The write is
// not checked for completion.
func (b Bridge) Write(t model.Target, m Msg, val uint64) error {
	lo, hi, split := b.split(m)
	if !split {
		return b.send(t, m, &val)
	}
	l, h := val&0xffffffff, val>>32
	if err := b.send(t, lo, &l); err != nil {
		return err
	}
	return b.send(t, hi, &h)
}
