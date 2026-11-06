// SPDX-License-Identifier: GPL-2.0-only

// Package jtag builds JTAG transactions as a flat stream of per-TCK commands
// and splits the captured TDO bits back into per-scan results. It knows
// nothing about the transport; anything implementing Driver can run it.
package jtag

import (
	"fmt"
	"math/big"
)

// TCK is a single clock cycle command.
type TCK uint8

const (
	TMS     TCK = 1 << 0 // TMS level for this clock
	TDI     TCK = 1 << 1 // TDI level for this clock
	Capture TCK = 1 << 2 // record TDO sampled on this clock
	// Wait holds this clock until the probe's trigger input fires (e.g. a
	// CPU signalling probe-mode ready), if the driver supports it.
	Wait TCK = 1 << 3
)

// Driver executes a TCK stream and returns the captured TDO bits, one per
// command that had Capture set, in order.
type Driver interface {
	Execute(cmds []TCK) ([]bool, error)
}

// State is a TAP controller state. Only the stable states a Builder can
// rest in are listed.
type State int

const (
	Unknown State = iota
	TestLogicReset
	RunTestIdle
	PauseDR // between ExchangeBegin and ExchangeEnd
)

// Scan is one IR or DR shift inside a Builder's transaction.
type Scan struct {
	IR   bool
	Bits int
	In   *big.Int
	Out  *big.Int // filled in by Run
	// Exchange scans shift Bits twice with a pause in between; In ends up
	// in the register and Out is the previous contents.
	Exchange bool
	offset   int // index of first captured bit
}

func (s *Scan) String() string {
	kind := "dr"
	if s.IR {
		kind = "ir"
	} else if s.Exchange {
		kind = "xdr"
	}
	in, out := "?", "?"
	if s.In != nil {
		in = fmt.Sprintf("%#x", s.In)
	}
	if s.Out != nil {
		out = fmt.Sprintf("%#x", s.Out)
	}
	return fmt.Sprintf("%s %d:%s -> %s", kind, s.Bits, in, out)
}

// Builder accumulates a TCK stream. Scans always start and end in
// Run-Test/Idle, which matches what OpenIPC does over DCI.
type Builder struct {
	// NoIRCapture skips capturing TDO during IR shifts (IR scans then have
	// no Out). Captured bits cost transport bandwidth.
	NoIRCapture bool

	cmds     []TCK
	scans    []*Scan
	captured int
	state    State
}

func NewBuilder() *Builder { return &Builder{state: Unknown} }

// NewBuilderInIdle returns a builder that assumes the TAP is already parked
// in Run-Test/Idle, so no Test-Logic-Reset is emitted. Use this for every
// transaction after the first: a TAP reset can drop state set up by earlier
// scans (e.g. TAP arbitration or wake requests).
func NewBuilderInIdle() *Builder { return &Builder{state: RunTestIdle} }

// NewBuilderInPause returns a builder for the transaction that continues an
// ExchangeBegin: the TAP is parked in Pause-DR.
func NewBuilderInPause() *Builder { return &Builder{state: PauseDR} }

func (b *Builder) clk(c TCK) { b.cmds = append(b.cmds, c) }

// Reset forces the TAP to Test-Logic-Reset (TMS high for 8 clocks) and then
// moves to Run-Test/Idle.
func (b *Builder) Reset() *Builder {
	for i := 0; i < 8; i++ {
		b.clk(TMS | TDI)
	}
	b.state = TestLogicReset
	return b.Idle(1)
}

// WaitTrigger adds one Run-Test/Idle clock that waits for the probe's
// trigger input, followed by one normal idle clock.
func (b *Builder) WaitTrigger() *Builder {
	if b.state != RunTestIdle {
		b.Idle(1)
	}
	b.clk(Wait)
	b.clk(TDI)
	return b
}

// Idle clocks n times in Run-Test/Idle (entering it if needed).
func (b *Builder) Idle(n int) *Builder {
	if b.state == PauseDR {
		panic("jtag: Idle in Pause-DR; finish the exchange with ExchangeEnd")
	}
	if b.state == Unknown {
		b.Reset()
	}
	for i := 0; i < n; i++ {
		b.clk(TDI)
	}
	if n > 0 {
		b.state = RunTestIdle
	}
	return b
}

func (b *Builder) shift(ir bool, bits int, in *big.Int) *Scan {
	if b.state != RunTestIdle {
		b.Idle(1)
	}
	if in == nil {
		in = new(big.Int)
	}
	// RTI -> Select-DR [-> Select-IR] -> Capture -> Shift
	b.clk(TMS | TDI)
	if ir {
		b.clk(TMS | TDI)
	}
	b.clk(TDI)
	b.clk(TDI)
	s := &Scan{IR: ir, Bits: bits, In: in, offset: b.captured}
	capture := !(ir && b.NoIRCapture)
	for i := 0; i < bits; i++ {
		var c TCK
		if capture {
			c = Capture
		}
		if in.Bit(i) != 0 {
			c |= TDI
		}
		if i == bits-1 {
			c |= TMS // last bit leaves Shift for Exit1
		}
		b.clk(c)
	}
	if capture {
		b.captured += bits
	} else {
		s.offset = -1
	}
	// Exit1 -> Update -> RTI
	b.clk(TMS | TDI)
	b.clk(TDI)
	b.state = RunTestIdle
	b.scans = append(b.scans, s)
	return s
}

// Exchange appends a DR scan that reads and replaces a register in one go:
// it shifts `bits` bits (capturing the old contents), pauses, then shifts the
// same number of bits again with `in`, which is what ends up in the register
// at Update-DR. Out holds the old contents. The first pass shifts `in` too.
func (b *Builder) Exchange(bits int, in *big.Int) *Scan {
	if b.state != RunTestIdle {
		b.Idle(1)
	}
	if in == nil {
		in = new(big.Int)
	}
	b.clk(TMS | TDI) // Select-DR
	b.clk(TDI)       // Capture-DR
	b.clk(TDI)       // Shift-DR
	s := &Scan{Bits: bits, In: in, offset: b.captured}
	for pass := 0; pass < 2; pass++ {
		for i := 0; i < bits; i++ {
			var c TCK
			if pass == 0 {
				c = Capture
			}
			if in.Bit(i) != 0 {
				c |= TDI
			}
			if i == bits-1 {
				c |= TMS // -> Exit1-DR
			}
			b.clk(c)
		}
		if pass == 0 {
			b.clk(TDI)       // Exit1 -> Pause
			b.clk(TMS | TDI) // Pause -> Exit2
			b.clk(TDI)       // Exit2 -> Shift
		}
	}
	s.Exchange = true
	b.captured += bits
	b.clk(TMS | TDI) // Exit1 -> Update
	b.clk(TDI)       // -> RTI
	b.state = RunTestIdle
	b.scans = append(b.scans, s)
	return s
}

// ExchangeBegin appends the first half of a read-modify-write DR scan: it
// shifts `bits` bits of `in` while capturing the old contents and stops in
// Pause-DR, without passing Update-DR. The transaction ends there; the
// next one (NewBuilderInPause) shifts the new value with ExchangeEnd.
func (b *Builder) ExchangeBegin(bits int, in *big.Int) *Scan {
	if b.state != RunTestIdle {
		b.Idle(1)
	}
	if in == nil {
		in = new(big.Int)
	}
	b.clk(TMS | TDI) // Select-DR
	b.clk(TDI)       // Capture-DR
	b.clk(TDI)       // Shift-DR
	s := &Scan{Bits: bits, In: in, offset: b.captured}
	for i := 0; i < bits; i++ {
		c := Capture
		if in.Bit(i) != 0 {
			c |= TDI
		}
		if i == bits-1 {
			c |= TMS // -> Exit1-DR
		}
		b.clk(c)
	}
	b.clk(TDI) // Exit1 -> Pause
	b.captured += bits
	b.state = PauseDR
	b.scans = append(b.scans, s)
	return s
}

// ExchangeEnd appends the second half of a read-modify-write DR scan: from
// Pause-DR it shifts `bits` bits of `in` (not captured), which end up in the
// register at Update-DR, and returns to Run-Test/Idle.
func (b *Builder) ExchangeEnd(bits int, in *big.Int) *Scan {
	if b.state != PauseDR {
		panic("jtag: ExchangeEnd outside Pause-DR")
	}
	if in == nil {
		in = new(big.Int)
	}
	b.clk(TMS | TDI) // Pause -> Exit2
	b.clk(TDI)       // Exit2 -> Shift
	s := &Scan{Bits: bits, In: in, offset: -1}
	for i := 0; i < bits; i++ {
		var c TCK
		if in.Bit(i) != 0 {
			c |= TDI
		}
		if i == bits-1 {
			c |= TMS // -> Exit1-DR
		}
		b.clk(c)
	}
	b.clk(TMS | TDI) // Exit1 -> Update
	b.clk(TDI)       // -> RTI
	b.state = RunTestIdle
	b.scans = append(b.scans, s)
	return s
}

// IR appends an instruction register scan of the given length.
func (b *Builder) IR(bits int, in *big.Int) *Scan { return b.shift(true, bits, in) }

// DR appends a data register scan of the given length.
func (b *Builder) DR(bits int, in *big.Int) *Scan { return b.shift(false, bits, in) }

// Commands returns the accumulated TCK stream.
func (b *Builder) Commands() []TCK { return b.cmds }

// Scans returns the scans in order.
func (b *Builder) Scans() []*Scan { return b.scans }

// Run executes the transaction and fills in each Scan's Out value.
func (b *Builder) Run(d Driver) error {
	tdo, err := d.Execute(b.cmds)
	if err != nil {
		return err
	}
	if len(tdo) < b.captured {
		return fmt.Errorf("jtag: driver returned %d TDO bits, expected %d", len(tdo), b.captured)
	}
	for _, s := range b.scans {
		if s.offset < 0 {
			continue
		}
		out := new(big.Int)
		for i := 0; i < s.Bits; i++ {
			if tdo[s.offset+i] {
				out.SetBit(out, i, 1)
			}
		}
		s.Out = out
	}
	return nil
}
