// SPDX-License-Identifier: GPL-2.0-only
package dci

import (
	"encoding/binary"
	"fmt"
)

// All ExI traffic on the DFX endpoints is made of fixed 64-byte packets.
// Several packets may be concatenated in a single USB bulk transfer.
const PacketSize = 64

// Packet types (byte 0).
const (
	TypeConfig = 0x01 // host->target: register writes; target->host: status
	TypeJtag   = 0x03 // host->target: TCK commands; target->host: TDO bits
)

// Byte 2 of an outbound packet.
const (
	flagStatus  = 0x01 // config: reply with a status packet
	flagJtagEnd = 0x03 // jtag: last packet of a scan, reply with TDO (+ status)
)

// Packet is one raw ExI packet.
type Packet [PacketSize]byte

func (p *Packet) Type() byte { return p[0] }
func (p *Packet) Seq() byte  { return p[1] }

// RegWrite is one register write inside a config packet.
type RegWrite struct {
	Reg   uint8
	Bank  uint8
	Value uint16
}

func (w RegWrite) String() string {
	return fmt.Sprintf("b%d.r%#x=%#04x", w.Bank, w.Reg, w.Value)
}

// maxWritesPerConfig is how many 4-byte register writes fit after the header.
const maxWritesPerConfig = (PacketSize - 4) / 4

func configPacket(seq byte, wantStatus bool, writes []RegWrite) Packet {
	var p Packet
	p[0] = TypeConfig
	p[1] = seq
	if wantStatus {
		p[2] = flagStatus
	}
	for i, w := range writes {
		o := 4 + 4*i
		p[o] = w.Reg
		p[o+1] = w.Bank
		binary.LittleEndian.PutUint16(p[o+2:], w.Value)
	}
	return p
}

// jtagNibblesPerPacket is how many TCK commands (4 bits each) fit in one
// packet after the 8-byte header: 56 bytes = 448 bits.
const jtagNibblesPerPacket = (PacketSize - 8) * 2

func jtagPacket(seq byte, last bool, cmds []uint8) Packet {
	var p Packet
	p[0] = TypeJtag
	p[1] = seq
	if last {
		p[2] = flagJtagEnd
	}
	p[3] = 0x80
	binary.LittleEndian.PutUint16(p[6:], uint16(len(cmds)*4))
	for i, c := range cmds {
		p[8+i/2] |= (c & 0xf) << (4 * (i % 2))
		if c&0x8 != 0 {
			p[3] |= 0x01 // packet contains trigger waits
		}
	}
	return p
}

// Status is a decoded target->host config/status packet.
type Status struct {
	Raw     Packet
	InSeq   byte // target's own outbound counter
	JtagAck byte // last JTAG packet sequence number processed
	CfgAck  byte // last config packet sequence number processed
}

func parseStatus(p Packet) Status {
	return Status{Raw: p, InSeq: p[1], JtagAck: p[2], CfgAck: p[3]}
}

// Word returns 16-bit register-dump word i (starting right after the header).
func (s Status) Word(i int) uint16 {
	return binary.LittleEndian.Uint16(s.Raw[4+2*i:])
}

// BankReg returns the value of a JTAG-engine (bank 1) register as echoed in
// the status dump; bank 1 registers 0..9 start at word 16.
func (s Status) BankReg(reg int) uint16 { return s.Word(16 + reg) }

// ArbitrationGranted reports whether our arbitration request is active.
func (s Status) ArbitrationGranted() bool { return s.Word(0) == 2 && s.Word(1) == 2 }

func (s Status) String() string {
	return fmt.Sprintf("status in=%02x jack=%02x cack=%02x % x", s.InSeq, s.JtagAck, s.CfgAck, s.Raw[4:56])
}

// TDO is a decoded target->host JTAG data packet.
type TDO struct {
	Raw  Packet
	Echo byte // sequence number of the host packet this answers
	Bits int
}

func parseTDO(p Packet) TDO {
	return TDO{Raw: p, Echo: p[2], Bits: int(binary.LittleEndian.Uint16(p[6:]))}
}

func (t TDO) Bit(i int) bool { return t.Raw[8+i/8]>>(i%8)&1 != 0 }
