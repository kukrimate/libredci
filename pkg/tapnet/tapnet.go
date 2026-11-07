// SPDX-License-Identifier: GPL-2.0-only

// Package tapnet turns accesses to named TAPs/registers of a model.Platform
// into JTAG transactions: it composes physical chains (other TAPs in
// BYPASS), splices child TAPs in and out, wraps accesses to virtual-chain
// TAPs, and manages preconditions.
package tapnet

import (
	"errors"
	"fmt"
	"log"
	"math/big"
	"slices"
	"time"

	"libredci/pkg/jtag"
	"libredci/pkg/model"
)

// Driver is a JTAG driver that can switch between the probe's chains.
type Driver interface {
	jtag.Driver
	SelectChain(n int) error
}

// TriggerDriver is implemented by drivers that can stall scans on a
// trigger input (Ops.WaitTrigger).
type TriggerDriver interface {
	SetTriggerMode(on bool) error
}

// Session holds the state of a debug session on one platform.
type Session struct {
	Plat    *model.Platform
	Drv     Driver
	Verbose bool
	// OnTransaction, if set, is called with every transaction's scans
	// after it has run (Out filled in).
	OnTransaction func(chain int, scans []*jtag.Scan)

	reset    map[int]bool // chain has been through TLR this session
	held     []heldCond   // acquired preconditions, in acquisition order
	acquired map[string]bool
	present  map[*model.TAP]bool // TAPs found present (cached for the session)
	absent   map[*model.TAP]bool // TAPs whose last presence check read absent
}

type heldCond struct {
	name    string
	release func(model.Target) error
}

func New(p *model.Platform, d Driver) *Session {
	return &Session{Plat: p, Drv: d, reset: map[int]bool{}, acquired: map[string]bool{},
		present: map[*model.TAP]bool{}, absent: map[*model.TAP]bool{}}
}

func (s *Session) Logf(format string, args ...any) {
	if s.Verbose {
		log.Printf("tapnet: "+format, args...)
	}
}

func (s *Session) Wait(d time.Duration) { time.Sleep(d) }

func (s *Session) TAP(name string) (*model.TAP, error) {
	t, ok := s.Plat.TAP(name)
	if !ok {
		return nil, fmt.Errorf("tapnet: no TAP named %q on %s", name, s.Plat.Name)
	}
	return t, nil
}

// ---- preconditions ----

func (s *Session) require(names []string) error {
	for _, n := range names {
		if s.acquired[n] {
			continue
		}
		c, ok := s.Plat.Conditions[n]
		if !ok {
			return fmt.Errorf("tapnet: unknown precondition %q", n)
		}
		if err := s.require(c.Requires); err != nil {
			return err
		}
		s.Logf("acquire %s", n)
		// Mark before acquiring so accesses made by the condition itself
		// don't recurse into it.
		s.acquired[n] = true
		rel, err := c.Acquire(s)
		if err != nil {
			delete(s.acquired, n)
			return fmt.Errorf("tapnet: precondition %s: %w", n, err)
		}
		s.held = append(s.held, heldCond{n, rel})
	}
	return nil
}

// ReleaseCondition undoes one held precondition; the next access that needs
// it acquires it again.
func (s *Session) ReleaseCondition(name string) error {
	for i, h := range s.held {
		if h.name != name {
			continue
		}
		s.Logf("release %s", h.name)
		s.held = slices.Delete(s.held, i, i+1)
		delete(s.acquired, h.name)
		if h.release != nil {
			return h.release(s)
		}
		return nil
	}
	return nil
}

// Release undoes all held preconditions in reverse order.
func (s *Session) Release() error {
	var errs []error
	for i := len(s.held) - 1; i >= 0; i-- {
		h := s.held[i]
		s.Logf("release %s", h.name)
		if h.release != nil {
			if err := h.release(s); err != nil {
				errs = append(errs, fmt.Errorf("release %s: %w", h.name, err))
			}
		}
		delete(s.acquired, h.name)
	}
	s.held = nil
	return errors.Join(errs...)
}

// ---- scan composition ----

// ctx knows how to reach the IR and DR of one TAP inside a transaction.
type ctx interface {
	// loadIR emits scans that leave `v` in this TAP's IR.
	loadIR(b *jtag.Builder, v *big.Int)
	// routeDR emits whatever is needed before this TAP's DR can be scanned.
	routeDR(b *jtag.Builder)
	// drLayout maps an n-bit DR access of this TAP onto the physical scan:
	// total length, value to shift, and how to extract the TAP's bits back.
	drLayout(n int, in *big.Int) (int, *big.Int, func(*big.Int) *big.Int)
}

func ones(n int) *big.Int {
	return new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), uint(n)), big.NewInt(1))
}

func bits(v *big.Int, lo, n int) *big.Int {
	if v == nil {
		return new(big.Int)
	}
	x := new(big.Int).Rsh(v, uint(lo))
	return x.And(x, ones(n))
}

// physical: a set of TAPs on the real chain, TDI side first; one is the target.
type physical struct {
	taps   []*model.TAP
	target int
}

func (p physical) loadIR(b *jtag.Builder, v *big.Int) {
	total, val := 0, new(big.Int)
	for i := len(p.taps) - 1; i >= 0; i-- { // TDO side is shifted first (LSB)
		n := p.taps[i].IRLen
		x := ones(n)
		if i == p.target {
			x = bits(v, 0, n)
		}
		val.Or(val, x.Lsh(x, uint(total)))
		total += n
	}
	b.IR(total, val)
}

func (p physical) routeDR(*jtag.Builder) {}

func (p physical) drLayout(n int, in *big.Int) (int, *big.Int, func(*big.Int) *big.Int) {
	off := len(p.taps) - 1 - p.target // BYPASS bits between target and TDO
	total := n + len(p.taps) - 1
	val := new(big.Int).Lsh(bits(in, 0, n), uint(off))
	return total, val, func(out *big.Int) *big.Int { return bits(out, off, n) }
}

// virtual: a TAP reached through its parent's IR/DR select instructions.
type virtual struct {
	parent ctx
	tap    *model.TAP
	link   model.Virtual
}

func (v virtual) loadIR(b *jtag.Builder, ir *big.Int) {
	v.parent.loadIR(b, new(big.Int).SetUint64(v.link.IRSelect))
	v.parent.routeDR(b)
	n := v.link.IRPadLo + v.tap.IRLen + v.link.IRPadHi
	total, val, _ := v.parent.drLayout(n, new(big.Int).Lsh(bits(ir, 0, v.tap.IRLen), uint(v.link.IRPadLo)))
	b.DR(total, val)
}

func (v virtual) routeDR(b *jtag.Builder) {
	v.parent.loadIR(b, new(big.Int).SetUint64(v.link.DRSelect))
	v.parent.routeDR(b)
}

func (v virtual) drLayout(n int, in *big.Int) (int, *big.Int, func(*big.Int) *big.Int) {
	inner := v.link.DRPadLo + n + v.link.DRPadHi
	total, val, ext := v.parent.drLayout(inner, new(big.Int).Lsh(bits(in, 0, n), uint(v.link.DRPadLo)))
	return total, val, func(out *big.Int) *big.Int { return bits(ext(out), v.link.DRPadLo, n) }
}

// spliceGroup returns t and the siblings spliced in with it (l.With), in
// chain order.
func (s *Session) spliceGroup(t *model.TAP, l model.Spliced) ([]*model.TAP, error) {
	group := []*model.TAP{t}
	for _, name := range l.With {
		w, err := s.TAP(name)
		if err != nil {
			return nil, err
		}
		wl, ok := w.Via.(model.Spliced)
		if !ok || w.Parent != t.Parent || wl.SelectIR != l.SelectIR || wl.Width != l.Width {
			return nil, fmt.Errorf("tapnet: %s: %s is not on the same select register", t.Name, name)
		}
		group = append(group, w)
	}
	slices.SortFunc(group, func(a, b *model.TAP) int {
		return a.Via.(model.Spliced).Bit - b.Via.(model.Spliced).Bit
	})
	return group, nil
}

// enter checks that tap and the TAPs above it are present, acquires their
// preconditions, selects the chain and returns a builder parked in
// Run-Test/Idle.
func (s *Session) enter(tap *model.TAP) (*jtag.Builder, error) {
	path := tap.Path()
	for _, t := range path {
		ok, err := s.Present(t)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, fmt.Errorf("tapnet: %s is not present on this target", t.Name)
		}
	}
	var reqs []string
	for _, t := range path {
		reqs = append(reqs, t.Requires...)
	}
	if err := s.require(reqs); err != nil {
		return nil, err
	}
	if err := s.Drv.SelectChain(tap.Chain); err != nil {
		return nil, err
	}
	var b *jtag.Builder
	if s.reset[tap.Chain] {
		b = jtag.NewBuilderInIdle()
	} else {
		b = jtag.NewBuilder()
		b.Reset()
		s.reset[tap.Chain] = true
	}
	b.NoIRCapture = true
	b.Idle(4)
	return b, nil
}

// run executes a builder's transaction and reports it.
func (s *Session) run(chain int, b *jtag.Builder) error {
	if err := b.Run(s.Drv); err != nil {
		return err
	}
	if s.OnTransaction != nil {
		s.OnTransaction(chain, b.Scans())
	}
	return nil
}

// access runs one transaction against tap: splice in what's needed, call
// body, splice back out.
func (s *Session) access(tap *model.TAP, body func(b *jtag.Builder, c ctx)) error {
	path := tap.Path()
	b, err := s.enter(tap)
	if err != nil {
		return err
	}

	type undo struct {
		chain  physical
		parent int
		link   model.Spliced
	}
	var undos []undo
	chain := physical{taps: []*model.TAP{path[0]}, target: 0}
	var c ctx = chain
	for _, t := range path[1:] {
		switch l := t.Via.(type) {
		case model.Spliced:
			pc, ok := c.(physical)
			if !ok {
				return fmt.Errorf("tapnet: %s: splicing below a virtual TAP is not supported", t.Name)
			}
			group, err := s.spliceGroup(t, l)
			if err != nil {
				return err
			}
			mask := new(big.Int)
			for _, g := range group {
				mask.SetBit(mask, g.Via.(model.Spliced).Bit, 1)
			}
			pc.loadIR(b, new(big.Int).SetUint64(l.SelectIR))
			total, val, _ := pc.drLayout(l.Width, mask)
			b.DR(total, val)
			b.Idle(4)
			// The group sits directly on the TDO side of its parent, ahead
			// of anything spliced in earlier further towards TDO.
			at := pc.target + 1
			taps := slices.Insert(slices.Clone(pc.taps), at, group...)
			target := at + slices.Index(group, t)
			undos = append(undos, undo{physical{taps: taps, target: pc.target}, pc.target, l})
			chain = physical{taps: taps, target: target}
			c = chain
		case model.Virtual:
			c = virtual{parent: c, tap: t, link: l}
		default:
			return fmt.Errorf("tapnet: %s: unknown link type %T", t.Name, t.Via)
		}
	}

	body(b, c)

	for i := len(undos) - 1; i >= 0; i-- {
		u := undos[i]
		b.Idle(4)
		u.chain.loadIR(b, new(big.Int).SetUint64(u.link.SelectIR))
		total, val, _ := u.chain.drLayout(u.link.Width, new(big.Int))
		b.DR(total, val)
	}
	b.Idle(4)
	return s.run(tap.Chain, b)
}

// Ops builds the scans of one transaction against a single TAP.
type Ops struct {
	b *jtag.Builder
	c ctx
}

// Result is a DR value captured by an Ops scan; valid after Do returns.
type Result struct {
	sc  *jtag.Scan
	ext func(*big.Int) *big.Int
}

func (r *Result) Value() *big.Int { return r.ext(r.sc.Out) }
func (r *Result) Uint64() uint64  { return r.Value().Uint64() }

// IR loads v into the TAP's instruction register (for some TAPs, passing
// through Update-IR with a given instruction is itself an action).
func (o *Ops) IR(v uint64) { o.c.loadIR(o.b, new(big.Int).SetUint64(v)) }

// DR scans the TAP's currently selected n-bit data register.
func (o *Ops) DR(n int, in *big.Int) *Result {
	o.c.routeDR(o.b)
	total, val, ext := o.c.drLayout(n, in)
	return &Result{o.b.DR(total, val), ext}
}

// Exchange is DR but captures the old contents and leaves `in` (see
// jtag.Builder.Exchange).
func (o *Ops) Exchange(n int, in *big.Int) *Result {
	o.c.routeDR(o.b)
	total, val, ext := o.c.drLayout(n, in)
	return &Result{o.b.Exchange(total, val), ext}
}

// IRDR is IR followed by DR.
func (o *Ops) IRDR(ir uint64, n int, in *big.Int) *Result {
	o.IR(ir)
	return o.DR(n, in)
}

// Idle clocks n times in Run-Test/Idle.
func (o *Ops) Idle(n int) { o.b.Idle(n) }

// WaitTrigger stalls in Run-Test/Idle until the probe's trigger input fires
// (needs a driver in trigger mode, see TriggerDriver).
func (o *Ops) WaitTrigger() { o.b.WaitTrigger() }

// Do runs fn's scans against tap as a single JTAG transaction.
func (s *Session) Do(tap *model.TAP, fn func(o *Ops)) error {
	return s.access(tap, func(b *jtag.Builder, c ctx) { fn(&Ops{b, c}) })
}

// IRDR loads `ir` into tap's IR and scans an n-bit DR with `in`, returning
// the captured n bits. This is the general raw access.
func (s *Session) IRDR(tap *model.TAP, ir uint64, n int, in *big.Int) (*big.Int, error) {
	var r *Result
	if err := s.Do(tap, func(o *Ops) { r = o.IRDR(ir, n, in) }); err != nil {
		return nil, err
	}
	return r.Value(), nil
}

// Scan accesses a modelled register (implements model.Target).
func (s *Session) Scan(tap *model.TAP, reg *model.Register, in *big.Int) (*big.Int, error) {
	if err := s.require(reg.Requires); err != nil {
		return nil, err
	}
	if in == nil {
		in = new(big.Int)
	}
	return s.IRDR(tap, reg.IR, reg.Bits, in)
}

// Read reads a register that can be read by scanning zeros through it.
func (s *Session) Read(tap *model.TAP, reg *model.Register) (*big.Int, error) {
	if reg.Access != model.ReadOnly && !reg.ZeroScanRead {
		return nil, fmt.Errorf("tapnet: %s.%s cannot be read without overwriting it (use Exchange)", tap.Name, reg.Name)
	}
	return s.Scan(tap, reg, nil)
}

// Exchange returns the old contents of reg and leaves `in` in it, in a
// single DR scan (shift, pause, shift).
func (s *Session) Exchange(tap *model.TAP, reg *model.Register, in *big.Int) (*big.Int, error) {
	if err := s.require(reg.Requires); err != nil {
		return nil, err
	}
	var r *Result
	if err := s.Do(tap, func(o *Ops) { o.IR(reg.IR); r = o.Exchange(reg.Bits, in) }); err != nil {
		return nil, err
	}
	return r.Value(), nil
}

// Modify read-modify-writes reg on a chain's root TAP in one DR scan split
// over two transactions: the first shifts fn(0) while capturing the old
// value and stops in Pause-DR; the second shifts fn(old) and updates the
// register. Only fn(old) is ever applied.
func (s *Session) Modify(tap *model.TAP, reg *model.Register, fn func(old *big.Int) *big.Int) (*big.Int, error) {
	if tap.Parent != nil {
		return nil, fmt.Errorf("tapnet: %s.%s: Modify needs a chain's root TAP", tap.Name, reg.Name)
	}
	if err := s.require(reg.Requires); err != nil {
		return nil, err
	}
	b, err := s.enter(tap)
	if err != nil {
		return nil, err
	}
	chain := physical{taps: []*model.TAP{tap}, target: 0}
	chain.loadIR(b, new(big.Int).SetUint64(reg.IR))
	sc := b.ExchangeBegin(reg.Bits, fn(new(big.Int)))
	if err := s.run(tap.Chain, b); err != nil {
		return nil, err
	}
	old := sc.Out
	b = jtag.NewBuilderInPause()
	b.NoIRCapture = true
	b.ExchangeEnd(reg.Bits, fn(new(big.Int).Set(old)))
	b.Idle(4)
	if err := s.run(tap.Chain, b); err != nil {
		return old, err
	}
	return old, nil
}

// presenceTimeout bounds how long Present waits for a valid existence bit.
const presenceTimeout = 2 * time.Second

// Present reports whether a TAP exists on this target, using its model's
// existence bit if it has one. A present result is cached for the session;
// an absent one is read again on the next call.
func (s *Session) Present(tap *model.TAP) (bool, error) {
	if tap.ExistsIn == nil {
		return true, nil
	}
	if s.present[tap] {
		return true, nil
	}
	owner, err := s.TAP(tap.ExistsIn.TAP)
	if err != nil {
		return false, err
	}
	reg := owner.MustRegister(tap.ExistsIn.Register)
	// Read until the validity field is set, for up to presenceTimeout.
	deadline := time.Now().Add(presenceTimeout)
	var v *big.Int
	for {
		var err error
		if v, err = s.Read(owner, reg); err != nil {
			return false, fmt.Errorf("tapnet: checking presence of %s: %w", tap.Name, err)
		}
		valid := true
		for name, want := range tap.ExistsIn.Valid {
			valid = valid && reg.MustField(name).Get(v) == want
		}
		if valid || time.Now().After(deadline) {
			break
		}
		time.Sleep(time.Millisecond)
	}
	ok := reg.MustField(tap.ExistsIn.Field).Get(v) != 0
	if ok {
		s.present[tap] = true
		delete(s.absent, tap)
	} else {
		s.absent[tap] = true
	}
	s.Logf("%s present: %v (%s = %#x)", tap.Name, ok, reg.Name, v)
	return ok, nil
}

// KnownAbsent reports whether a TAP's last presence check read absent.
func (s *Session) KnownAbsent(name string) bool {
	t, ok := s.Plat.TAP(name)
	return ok && s.absent[t]
}

// ReadIDCode reads the IDCODE of a TAP whose model has one.
func (s *Session) ReadIDCode(tap *model.TAP) (uint32, error) {
	if tap.IDCode == 0 {
		return 0, fmt.Errorf("tapnet: %s has no known IDCODE instruction", tap.Name)
	}
	v, err := s.IRDR(tap, tap.IDCodeIR, 32, nil)
	if err != nil {
		return 0, err
	}
	return uint32(v.Uint64()), nil
}
