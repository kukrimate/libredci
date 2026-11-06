// SPDX-License-Identifier: GPL-2.0-only

// Package model describes debug targets: which JTAG chains a platform has,
// the tree of TAPs on each chain, how child TAPs are reached from their
// parents, and the registers behind each TAP's instructions.
//
// Platform descriptions are plain Go values (see package platforms/...).
package model

import (
	"fmt"
	"math/big"
	"strings"
	"time"
)

// Field is a named bit range [Lo, Hi] inside a register.
type Field struct {
	Name   string
	Lo, Hi int
}

func (f Field) Get(v *big.Int) uint64 {
	w := uint(f.Hi - f.Lo + 1)
	x := new(big.Int).Rsh(v, uint(f.Lo))
	x.And(x, new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), w), big.NewInt(1)))
	return x.Uint64()
}

func (f Field) Set(v *big.Int, val uint64) *big.Int {
	w := uint(f.Hi - f.Lo + 1)
	mask := new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), w), big.NewInt(1))
	out := new(big.Int).Set(v)
	out.AndNot(out, new(big.Int).Lsh(mask, uint(f.Lo)))
	x := new(big.Int).And(new(big.Int).SetUint64(val), mask)
	return out.Or(out, x.Lsh(x, uint(f.Lo)))
}

// Access says what scanning a register does.
type Access int

const (
	// ReadOnly registers can be read by scanning zeros through them.
	ReadOnly Access = iota
	// ReadWrite registers take whatever is shifted in at Update-DR, so a
	// read must also write (see tapnet.Session.Exchange).
	ReadWrite
	// WriteOnly registers do not return meaningful data.
	WriteOnly
)

// Register is a data register selected by an IR opcode.
type Register struct {
	Name     string
	IR       uint64
	Bits     int
	Access   Access
	Fields   []Field
	Requires []string // preconditions needed to access it (in addition to the TAP's)
	// ZeroScanRead marks a ReadWrite register whose established read method
	// is scanning zeros through it (zero is its normal writable content).
	ZeroScanRead bool
}

func (r *Register) Field(name string) (Field, bool) {
	for _, f := range r.Fields {
		if f.Name == name {
			return f, true
		}
	}
	return Field{}, false
}

// MustField is Field for names known at compile time.
func (r *Register) MustField(name string) Field {
	f, ok := r.Field(name)
	if !ok {
		panic(fmt.Sprintf("model: register %s has no field %s", r.Name, name))
	}
	return f
}

// Decode renders a value field by field.
func (r *Register) Decode(v *big.Int) string {
	var sb strings.Builder
	for _, f := range r.Fields {
		fmt.Fprintf(&sb, "  %-28s [%d:%d] = %#x\n", f.Name, f.Hi, f.Lo, f.Get(v))
	}
	return sb.String()
}

// ExistsBit locates a presence flag. Valid lists field values of the same
// register that together mean the register holds a valid value.
type ExistsBit struct {
	TAP, Register, Field string
	Valid                map[string]uint64
}

// Thread describes a hardware thread debuggable through a TAP. Arch selects
// the run-control implementation (see package runctl).
type Thread struct {
	Name string
	Arch string
	// Index distinguishes threads sharing a TAP.
	Index int
}

// Link describes how a TAP is reached from its parent.
type Link interface{ isLink() }

// Spliced: the parent has a select register (instruction SelectIR, Width
// bits wide); setting bit Bit inserts the child TAP into the scan chain right
// after the parent (on the TDO side). Writing zero removes it again.
//
// With names sibling TAPs on the same select register that are inserted
// together with the child. The inserted TAPs are in select-bit order, the
// highest bit nearest TDO.
type Spliced struct {
	SelectIR uint64
	Width    int
	Bit      int
	With     []string
}

// Virtual: the child is not on the physical chain. Its IR is written by
// scanning the parent's IRSelect instruction and shifting the child IR
// through the parent's DR; its DR is accessed through the parent's DRSelect
// instruction. The Pad fields are extra bits on the low (TDO, shifted out
// first) and high side of those parent DR scans.
type Virtual struct {
	IRSelect, DRSelect uint64
	IRPadLo, IRPadHi   int
	DRPadLo, DRPadHi   int
}

func (Spliced) isLink() {}
func (Virtual) isLink() {}

// TAP is one TAP controller.
type TAP struct {
	Name     string
	IRLen    int
	IDCode   uint32 // expected IDCODE, 0 if unknown
	IDCodeIR uint64 // instruction selecting IDCODE, valid if IDCode != 0
	Optional bool   // may be fused off / absent on some parts
	Via      Link   // nil for the TAP at the root of a chain
	Requires []string
	// ExistsIn names a register field (on another TAP) that says whether
	// this TAP exists on the part at hand.
	ExistsIn *ExistsBit
	// Threads are the CPU threads controlled through this TAP.
	Threads   []Thread
	Registers []*Register
	Children  []*TAP

	Parent *TAP // filled in by Platform.Init
	Chain  int  // filled in by Platform.Init
}

func (t *TAP) Register(name string) (*Register, bool) {
	for _, r := range t.Registers {
		if r.Name == name {
			return r, true
		}
	}
	return nil, false
}

func (t *TAP) MustRegister(name string) *Register {
	r, ok := t.Register(name)
	if !ok {
		panic(fmt.Sprintf("model: TAP %s has no register %s", t.Name, name))
	}
	return r
}

// Path returns the TAPs from the chain root down to t.
func (t *TAP) Path() []*TAP {
	var p []*TAP
	for x := t; x != nil; x = x.Parent {
		p = append([]*TAP{x}, p...)
	}
	return p
}

// IDCodeMatches compares ignoring the 4-bit version field, which differs
// between steppings.
func (t *TAP) IDCodeMatches(v uint32) bool {
	return t.IDCode != 0 && v&0x0fffffff == t.IDCode&0x0fffffff
}

// Target is what precondition procedures use to talk to the hardware.
// tapnet.Session implements it.
type Target interface {
	TAP(name string) (*TAP, error)
	// Scan writes `in` to register reg of tap and returns what was captured.
	Scan(tap *TAP, reg *Register, in *big.Int) (*big.Int, error)
	// Exchange captures the old value of reg and leaves `in` in it.
	Exchange(tap *TAP, reg *Register, in *big.Int) (*big.Int, error)
	// Modify read-modify-writes reg via fn; guess is the expected current
	// value (the register can only be read by also writing it).
	Modify(tap *TAP, reg *Register, guess *big.Int, fn func(old *big.Int) *big.Int) (*big.Int, error)
	Wait(d time.Duration)
	Logf(format string, args ...any)
	// KnownAbsent reports whether a TAP has been found not to exist.
	KnownAbsent(name string) bool
}

// Condition is a precondition: target state that must be set up before
// some accesses and torn down afterwards (e.g. "TAP arbitrated", "package
// awake"). Acquire returns the function that undoes it.
type Condition struct {
	Name     string
	Requires []string // conditions to acquire first
	Acquire  func(t Target) (release func(Target) error, err error)
}

// Chain is one physical JTAG chain reachable through the probe.
type Chain struct {
	Index int
	Root  *TAP
}

// Platform is a complete target description.
type Platform struct {
	Name       string
	Chains     []Chain
	Conditions map[string]*Condition

	byName map[string]*TAP
}

// Init links parents and indexes TAPs by name. Call once after construction.
func (p *Platform) Init() *Platform {
	p.byName = map[string]*TAP{}
	var walk func(t *TAP, parent *TAP, chain int)
	walk = func(t *TAP, parent *TAP, chain int) {
		t.Parent, t.Chain = parent, chain
		if _, dup := p.byName[t.Name]; dup {
			panic("model: duplicate TAP name " + t.Name)
		}
		p.byName[t.Name] = t
		for _, c := range t.Children {
			walk(c, t, chain)
		}
	}
	for _, c := range p.Chains {
		walk(c.Root, nil, c.Index)
	}
	return p
}

func (p *Platform) TAP(name string) (*TAP, bool) {
	t, ok := p.byName[name]
	return t, ok
}

// Walk visits every TAP depth-first.
func (p *Platform) Walk(fn func(t *TAP, depth int)) {
	var walk func(t *TAP, d int)
	walk = func(t *TAP, d int) {
		fn(t, d)
		for _, c := range t.Children {
			walk(c, d+1)
		}
	}
	for _, c := range p.Chains {
		walk(c.Root, 0)
	}
}
