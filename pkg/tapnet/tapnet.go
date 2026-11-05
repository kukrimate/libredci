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
	lastSeen map[*model.Register]*big.Int
	present  map[*model.TAP]bool
}

type heldCond struct {
	name    string
	release func(model.Target) error
}

func New(p *model.Platform, d Driver) *Session {
	return &Session{Plat: p, Drv: d, reset: map[int]bool{}, acquired: map[string]bool{},
		present: map[*model.TAP]bool{}}
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

// access runs one transaction against tap: splice in what's needed, call
// body, splice back out.
func (s *Session) access(tap *model.TAP, body func(b *jtag.Builder, c ctx)) error {
	path := tap.Path()
	for _, t := range path {
		ok, err := s.Present(t)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("tapnet: %s is not present on this target", t.Name)
		}
	}
	var reqs []string
	for _, t := range path {
		reqs = append(reqs, t.Requires...)
	}
	if err := s.require(reqs); err != nil {
		return err
	}
	if err := s.Drv.SelectChain(tap.Chain); err != nil {
		return err
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
			taps := slices.Clone(pc.taps)
			target := 0
			for _, g := range group {
				if g == t {
					target = len(taps)
				}
				taps = append(taps, g)
			}
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
	if err := b.Run(s.Drv); err != nil {
		return err
	}
	if s.OnTransaction != nil {
		s.OnTransaction(tap.Chain, b.Scans())
	}
	return nil
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
	v, err := s.IRDR(tap, reg.IR, reg.Bits, in)
	if err == nil {
		s.seen(tap, reg, v)
	}
	return v, err
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
	old := r.Value()
	s.seen(tap, reg, in)
	return old, nil
}

func (s *Session) seen(tap *model.TAP, reg *model.Register, v *big.Int) {
	if s.lastSeen == nil {
		s.lastSeen = map[*model.Register]*big.Int{}
	}
	s.lastSeen[reg] = new(big.Int).Set(v)
}

// Modify read-modify-writes reg. JTAG can only read such a register by
// also writing it, so it writes fn(guess) while capturing the old value in
// the same scan, and rewrites fn(old) if that differs. The guess is the
// last value this session saw in the register, else the caller's.
func (s *Session) Modify(tap *model.TAP, reg *model.Register, guess *big.Int, fn func(old *big.Int) *big.Int) (old *big.Int, err error) {
	if last, ok := s.lastSeen[reg]; ok {
		guess = last
	}
	first := fn(guess)
	old, err = s.Exchange(tap, reg, first)
	if err != nil {
		return nil, err
	}
	if want := fn(old); want.Cmp(first) != 0 {
		s.Logf("%s.%s: guess %#x wrong (was %#x), rewriting %#x", tap.Name, reg.Name, guess, old, want)
		if _, err := s.Exchange(tap, reg, want); err != nil {
			return old, err
		}
	}
	return old, nil
}

// Present reports whether a TAP exists on this target, using its model's
// existence bit if it has one (the result is cached for the session).
func (s *Session) Present(tap *model.TAP) (bool, error) {
	if tap.ExistsIn == nil {
		return true, nil
	}
	if v, ok := s.present[tap]; ok {
		return v, nil
	}
	owner, err := s.TAP(tap.ExistsIn.TAP)
	if err != nil {
		return false, err
	}
	reg := owner.MustRegister(tap.ExistsIn.Register)
	v, err := s.Read(owner, reg)
	if err != nil {
		return false, fmt.Errorf("tapnet: checking presence of %s: %w", tap.Name, err)
	}
	ok := reg.MustField(tap.ExistsIn.Field).Get(v) != 0
	s.present[tap] = ok
	s.Logf("%s present: %v", tap.Name, ok)
	return ok, nil
}

// KnownAbsent reports whether a TAP has been found to be absent.
func (s *Session) KnownAbsent(name string) bool {
	t, ok := s.Plat.TAP(name)
	if !ok {
		return false
	}
	p, known := s.present[t]
	return known && !p
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
