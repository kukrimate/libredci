// SPDX-License-Identifier: GPL-2.0-only

// Package bigcore implements run control for Intel "big" cores (Skylake /
// Kaby Lake generation) through their core TAP's probe mode.
//
// While a core is in probe mode, the debugger feeds it x86 instructions one
// at a time (sub_pir_write, IR 0x83) and the core executes them in the
// halted thread's own context and mode. Results are moved into the probe
// data register (readpdr, IR 0x84/0x85 per thread) with special probe-mode
// MSR accesses. On entry the microcode saves RCX, RDX, RIP and RAX in
// scratch registers SCP1/3/7/8 and restores them from there on exit; the
// debugger saves and restores the other state its instructions touch.
package bigcore

import (
	"errors"
	"fmt"
	"math/big"
	"sync"
	"time"

	"libredci/pkg/model"
	"libredci/pkg/runctl"
	"libredci/pkg/tapnet"
)

// Core TAP instructions.
const (
	irTapStatus = 0x60
	irSubPIR    = 0x83 // 134 bits: [3:0] 0, [131:4] 16 code bytes, [132] thread, [133] 0
	irReadPDR0  = 0x84 // 66 bits: [64:1] data (thread 0)
	irProbeMode = 0x86 // 4 bits: [1] probe-mode entry request
)

// Probe-mode MSR indices.
const (
	msrProbeToPDR = 0x70  // rdmsr with EAX = index: scratch/state value -> PDR
	msrProbeGPReg = 0x71  // rdmsr: EAX -> PDR
	msrProbeSeg   = 0x72  // rdmsr with EAX = segment: base -> PDR, rest -> scratch
	msrP6CRCPL    = 0x101 // current privilege level
	msrSCP0       = 0x160 // scratch registers SCP0..SCP15
	msrICECTLPMR  = 0x1df // probe-mode control
	icePDR        = 1 << 14
	iceFault      = 1 << 4
	iceRemInPM    = 1 << 17
)

// tap_status bits.
const (
	tsRedirT1    = 1 << 7 // entered probe mode through a redirected exception
	tsRedirT0    = 1 << 8
	tsPMRdyT1    = 1 << 5
	tsPMRdyT0    = 1 << 6
	tsDisabledT1 = 1 << 13
	tsDisabledT0 = 1 << 14
)

// Segment numbers for msrProbeSeg.
var segNum = map[string]int{"es": 1, "cs": 3, "ss": 5, "ds": 7, "fs": 9, "gs": 0xb, "idt": 0xc, "gdt": 0xd, "tr": 0xe, "ldt": 0xf}

func init() {
	runctl.Register("kbl-core", func(s *tapnet.Session, tap *model.TAP, th model.Thread) (runctl.Thread, error) {
		uncore := tap.Parent
		if uncore == nil {
			return nil, fmt.Errorf("bigcore: %s has no parent TAP", tap.Name)
		}
		var coreID int
		if _, err := fmt.Sscanf(tap.Name[len(uncore.Name):], ".core%d", &coreID); err != nil {
			return nil, fmt.Errorf("bigcore: cannot derive core number from %s", tap.Name)
		}
		return &Thread{s: s, core: coreFor(s, tap, uncore, coreID), idx: th.Index, name: th.Name}, nil
	})
}

// coreState is shared by the threads of one core: halting stops both.
type coreState struct {
	tap, uncore *model.TAP
	id          int
	halted      bool
	wakeOld     *big.Int
	threads     map[int]*Thread // threads prepared for probe-mode instructions
}

var (
	coresMu sync.Mutex
	cores   = map[*tapnet.Session]map[*model.TAP]*coreState{}
)

func coreFor(s *tapnet.Session, tap, uncore *model.TAP, id int) *coreState {
	coresMu.Lock()
	defer coresMu.Unlock()
	m := cores[s]
	if m == nil {
		m = map[*model.TAP]*coreState{}
		cores[s] = m
	}
	c := m[tap]
	if c == nil {
		c = &coreState{tap: tap, uncore: uncore, id: id, threads: map[int]*Thread{}}
		m[tap] = c
	}
	return c
}

// Thread is one hardware thread of a big core.
type Thread struct {
	s    *tapnet.Session
	core *coreState
	idx  int
	name string

	prepared bool
	mode     int // 16, 32, 64
	entry    uint64
	scp      [11]uint64 // SCP1..SCP10 at entry
	rbx      uint64
	rflags   uint64
	cpl      uint64
	dr6, dr7 uint64
	icectl   uint64
	dr6Set   bool       // user wrote dr6: restore it instead of the clean value
	flat     *flatState // non-nil while switched to 64-bit mode for memory access

	bps              [4]*breakpoint
	usedSlot         [4]bool
	stepping         bool
	progDR7, progICE uint64 // program's own values while we use them
	progSaved        bool
}

func (t *Thread) Name() string { return t.name }

func (t *Thread) tapStatus() (uint64, error) {
	v, err := t.s.IRDR(t.core.tap, irTapStatus, 37, nil)
	if err != nil {
		return 0, err
	}
	return v.Uint64(), nil
}

func (t *Thread) inPM(st uint64) bool {
	rdy := uint64(tsPMRdyT0)
	if t.idx == 1 {
		rdy = tsPMRdyT1
	}
	return st&rdy != 0
}

func (t *Thread) Halted() (bool, error) {
	st, err := t.tapStatus()
	if err != nil {
		return false, err
	}
	return t.inPM(st), nil
}

// ---- probe-mode instruction execution ----

type prog struct {
	t   *Thread
	o   *tapnet.Ops
	out []*tapnet.Result
}

// exec submits instructions one at a time, waiting for each to complete.
func (p *prog) exec(insns ...[]byte) {
	for _, in := range insns {
		var line [16]byte
		for i := range line {
			line[i] = 0x90
		}
		copy(line[:], in)
		v := new(big.Int)
		for i := 15; i >= 0; i-- {
			v.Lsh(v, 8)
			v.Or(v, big.NewInt(int64(line[i])))
		}
		v.Lsh(v, 4)
		if p.t.idx == 1 {
			v.SetBit(v, 132, 1)
		}
		p.o.IRDR(irSubPIR, 134, v)
		p.o.WaitTrigger()
	}
}

// pdr queues a probe-data-register read and returns its index in out.
func (p *prog) pdr() int {
	p.out = append(p.out, p.o.IRDR(uint64(irReadPDR0+p.t.idx), 66, nil))
	return len(p.out) - 1
}

func (t *Thread) run(fn func(p *prog, e enc)) ([]uint64, error) {
	var p *prog
	err := t.s.Do(t.core.tap, func(o *tapnet.Ops) {
		p = &prog{t: t, o: o}
		fn(p, enc{t.mode})
	})
	if err != nil {
		return nil, err
	}
	out := make([]uint64, len(p.out))
	for i, r := range p.out {
		out[i] = new(big.Int).Rsh(r.Value(), 1).Uint64() // data is [64:1]
	}
	return out, nil
}

// one runs a sequence and returns the single PDR value it reads.
func (t *Thread) one(fn func(p *prog, e enc)) (uint64, error) {
	v, err := t.run(func(p *prog, e enc) { fn(p, e); p.pdr() })
	if err != nil {
		return 0, err
	}
	return v[0], nil
}

// ---- halt / resume ----

func (t *Thread) trigger(on bool) error {
	if td, ok := t.s.Drv.(tapnet.TriggerDriver); ok {
		return td.SetTriggerMode(on)
	}
	if on {
		return errors.New("bigcore: probe driver has no trigger support")
	}
	return nil
}

func (t *Thread) Halt() (runctl.StopInfo, error) {
	c := t.core
	if !c.halted {
		st, err := t.tapStatus()
		if err != nil {
			return runctl.StopInfo{}, err
		}
		if t.inPM(st) && (t.Armed() || t.stepping) {
			return t.stopped() // stopped at a breakpoint or step we armed
		}
		redir := uint64(tsRedirT0)
		if t.idx == 1 {
			redir = tsRedirT1
		}
		if t.inPM(st) && st&redir != 0 {
			// Stopped by a debug exception redirected into probe mode, armed
			// by an earlier session: take over and drop the debug settings
			// (trap flag, redirection) so it doesn't trap again on resume.
			if err := t.wholeCore(); err != nil {
				return runctl.StopInfo{}, err
			}
			c.halted = true
			if err := t.prepare(); err != nil {
				return runctl.StopInfo{}, err
			}
			t.rflags &^= eflTF | eflRF
			t.icectl &^= iceIR
			t.progDR7, t.progICE = t.dr7, t.icectl
			return runctl.StopInfo{PC: t.pc(), Reason: "debug exception (taken over)"}, nil
		}
		if t.inPM(st) {
			// Requested probe mode (not a debug exception) by someone else:
			// nothing has been changed yet, so take it over as a halt.
			if err := t.wholeCore(); err != nil {
				return runctl.StopInfo{}, err
			}
			c.halted = true
			if err := t.prepare(); err != nil {
				return runctl.StopInfo{}, err
			}
			return runctl.StopInfo{PC: t.pc(), Reason: "already in probe mode"}, nil
		}
		// Let pcode handle the probe-mode request for this core.
		wake, _ := c.uncore.Register("pcode_wake_req")
		old, err := t.s.Modify(c.uncore, wake, func(v *big.Int) *big.Int {
			v = wake.MustField("probe_mode_pcode_flow").Set(v, 1)
			pre := wake.MustField("core_preq_enable")
			return pre.Set(v, pre.Get(v)|1<<c.id)
		})
		if err != nil {
			return runctl.StopInfo{}, err
		}
		c.wakeOld = old
		if _, err := t.s.IRDR(c.tap, irProbeMode, 4, big.NewInt(2)); err != nil {
			return runctl.StopInfo{}, err
		}
		deadline := time.Now().Add(time.Second)
		for {
			st, err := t.tapStatus()
			if err != nil {
				return runctl.StopInfo{}, err
			}
			rdy := st&(tsPMRdyT0|tsDisabledT0) != 0 && st&(tsPMRdyT1|tsDisabledT1) != 0
			if rdy {
				break
			}
			if time.Now().After(deadline) {
				t.s.IRDR(c.tap, irProbeMode, 4, big.NewInt(0))
				return runctl.StopInfo{}, fmt.Errorf("bigcore: core %d did not enter probe mode (tap_status %#x); is debug consent enabled?", c.id, st)
			}
			time.Sleep(time.Millisecond)
		}
		c.halted = true
	}
	if err := t.prepare(); err != nil {
		return runctl.StopInfo{}, err
	}
	return runctl.StopInfo{PC: t.pc(), Reason: "halt request"}, nil
}

// prepare saves this thread's state and sets up probe-mode execution.
func (t *Thread) prepare() error {
	if t.prepared {
		return nil
	}
	if err := t.trigger(true); err != nil {
		return err
	}
	// Entry snapshot: EFER [15:0], CS [47:32], CS attributes [63:48].
	v, err := t.run(func(p *prog, _ enc) { p.pdr() })
	if err != nil {
		return err
	}
	entry := v[0]
	if !plausibleEntry(entry) {
		// No entry snapshot (e.g. stopped by a redirected debug exception):
		// derive CS from the segment state with mode-independent code.
		if entry, err = t.synthEntry(); err != nil {
			return err
		}
	}
	if entry&(1<<31) != 0 {
		return fmt.Errorf("bigcore: %s: thread is in an unsupported state (entry snapshot %#x)", t.name, entry)
	}
	efer, csar := entry&0xffff, entry>>48&0xffff
	t.entry = entry
	t.mode = 16
	if csar&(1<<14) != 0 {
		t.mode = 32
	}
	if efer&(1<<10) != 0 && csar&(1<<13) != 0 {
		t.mode = 64
	}

	// Save state, then set up the probe environment.
	vals, err := t.run(func(p *prog, e enc) {
		// ECX := 0 using only mode-independent encodings.
		p.exec(e.movR8Imm(rCL, 0), e.movChCl(), e.bswapECX32(), e.bswapCX16(), e.movR8Imm(rCL, 0), e.movChCl())
		for i := 1; i <= 10; i++ { // SCP1..10
			p.exec(e.movR8Imm(rCL, msrProbeToPDR), e.movR8Imm(rCH, 0), e.movR8Imm(rAL, byte(i)), e.movR8Imm(rAH, 0), e.rdmsr())
			p.pdr()
		}
		// RBX
		p.exec(e.movRR(rAX, rBX), e.movR32Imm(rCX, msrProbeGPReg), e.rdmsr())
		p.pdr()
		// RFLAGS: saved part in SCP2, live arithmetic flags via lahf/seto.
		p.exec(e.movR32Imm(rCX, msrProbeGPReg), e.rdmsr(), e.movR32Imm(rCX, msrSCP0+2), e.rdmsr(),
			e.movRR(rDX, rAX), e.movImm(rCX, 0), e.setoCL(), e.movImm(rAX, 0), e.lahf(),
			e.shrImm(rAX, 8), e.orRR(rDX, rAX), e.shlImm(rCX, 0xb), e.orRR(rDX, rCX), e.movRR(rAX, rDX),
			e.movR32Imm(rCX, msrProbeGPReg), e.rdmsr())
		p.pdr()
		// CPL
		p.exec(e.movR32Imm(rCX, msrP6CRCPL), e.rdmsr(), e.movR32Imm(rDX, 0), e.andEAXImm(3),
			e.movR32Imm(rCX, msrProbeGPReg), e.rdmsr())
		p.pdr()
		// DR6
		p.exec(e.movFromDR(rAX, 6), e.movR32Imm(rCX, msrProbeGPReg), e.rdmsr())
		p.pdr()
		// ICECTLPMR (low half holds all fields)
		p.exec(e.movR32Imm(rCX, msrICECTLPMR), e.rdmsr(), e.movR32Imm(rCX, msrProbeGPReg), e.rdmsr())
		p.pdr()
		// DR7
		p.exec(e.movFromDR(rAX, 7), e.movR32Imm(rCX, msrProbeGPReg), e.rdmsr())
		p.pdr()
	})
	if err != nil {
		return fmt.Errorf("bigcore: %s: saving state: %w", t.name, err)
	}
	copy(t.scp[1:], vals[0:10])
	t.rbx, t.rflags, t.cpl, t.dr6, t.icectl, t.dr7 = vals[10], vals[11], vals[12], vals[13], vals[14], vals[15]
	if !t.progSaved {
		t.progDR7, t.progICE, t.progSaved = t.dr7, t.icectl, true
	}
	t.prepared = true
	t.core.threads[t.idx] = t

	_, err = t.run(func(p *prog, e enc) {
		// CPL 0, PDR mode off, DR7 = 0x400 (no breakpoints) while debugging.
		p.exec(e.movR32Imm(rCX, msrP6CRCPL), e.movR32Imm(rAX, 0), e.movR32Imm(rDX, 0), e.wrmsr())
		p.exec(e.movR32Imm(rCX, msrICECTLPMR), e.movR32Imm(rAX, uint32(t.icectl)&^icePDR), e.movR32Imm(rDX, uint32(t.icectl>>32)), e.wrmsr())
		p.exec(e.movImm(rAX, 0x400), e.movToDR(7, rAX))
	})
	if err != nil {
		return fmt.Errorf("bigcore: %s: probe setup: %w", t.name, err)
	}
	return nil
}

func plausibleEntry(entry uint64) bool {
	csar := entry >> 48 & 0xffff
	return entry != 0 && entry != ^uint64(0) && csar&(1<<4) != 0 && csar&(1<<7) != 0 &&
		!(csar&(1<<13) != 0 && csar&(1<<14) != 0)
}

// synthEntry builds an entry snapshot from the live CS state. It only uses
// byte-register instructions, whose encoding is the same in every mode,
// and preserves SCP1/SCP2 (which hold the program's RCX and part of RFLAGS).
func (t *Thread) synthEntry() (uint64, error) {
	e := enc{16} // only mode-independent encodings below
	clearECX := func(p *prog) {
		p.exec(e.movR8Imm(rCL, 0), e.movChCl(), []byte{0x66, 0x0f, 0xc9}, []byte{0x0f, 0xc9}, e.movR8Imm(rCL, 0), e.movChCl())
	}
	scp := func(p *prog, i byte) {
		p.exec(e.movR8Imm(rCL, msrProbeToPDR), e.movR8Imm(rCH, 0), e.movR8Imm(rAL, i), e.movR8Imm(rAH, 0), e.rdmsr())
		p.pdr()
	}
	v, err := t.run(func(p *prog, _ enc) {
		clearECX(p)
		scp(p, 1)
		scp(p, 2)
		p.exec(e.movR8Imm(rCL, msrProbeSeg), e.movR8Imm(rCH, 0), e.movR8Imm(rAL, segCS), e.movR8Imm(rAH, 0), e.rdmsr())
		scp(p, 1)
		scp(p, 2)
	})
	if err != nil {
		return 0, err
	}
	scp1, scp2, csinfo, cssel := v[0], v[1], v[2], v[3]
	csar := csinfo >> 16 & 0xffff
	entry := csar<<48 | (cssel&0xffff)<<32
	if csar&(1<<13) != 0 {
		entry |= 1<<10 | 1<<8 // long mode active and enabled
	}
	if !plausibleEntry(entry) {
		return 0, fmt.Errorf("bigcore: %s: cannot determine the thread's mode (CS info %#x)", t.name, csinfo)
	}
	// Put SCP1/SCP2 back now that the mode is known.
	mode := 16
	if csar&(1<<14) != 0 {
		mode = 32
	}
	if csar&(1<<13) != 0 {
		mode = 64
	}
	err = t.s.Do(t.core.tap, func(o *tapnet.Ops) {
		p := &prog{t: t, o: o}
		e := enc{mode}
		for i, val := range []uint64{scp1, scp2} {
			p.exec(e.movR32Imm(rCX, uint32(msrSCP0+1+i)), e.movR32Imm(rAX, uint32(val)), e.movR32Imm(rDX, uint32(val>>32)), e.wrmsr())
		}
	})
	return entry, err
}

// restore puts back everything prepare saved, in the reference order.
func (t *Thread) restore() error {
	if err := t.leaveFlat64(); err != nil {
		return err
	}
	dr7, ice, rflags, err := t.debugOverlay()
	if err != nil {
		return err
	}
	dr6 := uint64(0xffff0ff0)
	if t.dr6Set {
		dr6 = t.dr6
	}
	_, err = t.run(func(p *prog, e enc) {
		p.exec(e.movImm(rAX, dr6), e.movToDR(6, rAX))
		p.exec(e.movImm(rAX, dr7), e.movToDR(7, rAX))
		p.exec(e.movR32Imm(rCX, msrICECTLPMR), e.movR32Imm(rAX, uint32(ice)), e.movR32Imm(rDX, uint32(ice>>32)), e.wrmsr())
		p.exec(e.movImm(rAX, rflags), e.movR32Imm(rDX, uint32(rflags>>32)), e.movR32Imm(rCX, msrProbeGPReg), e.rdmsr(),
			e.movR32Imm(rCX, msrSCP0+2), e.wrmsr(), e.movR32Imm(rCX, msrProbeGPReg), e.wrmsr())
		p.exec(e.movR32Imm(rCX, msrP6CRCPL), e.movR32Imm(rAX, uint32(t.cpl)), e.movR32Imm(rDX, 0), e.wrmsr())
		p.exec(e.movImm(rBX, t.rbx))
		for i := 1; i <= 10; i++ {
			p.exec(e.movR32Imm(rCX, uint32(msrSCP0+i)), e.movR32Imm(rAX, uint32(t.scp[i])), e.movR32Imm(rDX, uint32(t.scp[i]>>32)), e.wrmsr())
		}
	})
	return err
}

func (t *Thread) Resume() error {
	c := t.core
	if !c.halted {
		return fmt.Errorf("bigcore: %s is not halted (by us)", t.name)
	}
	for _, th := range c.threads {
		if err := th.restore(); err != nil {
			return fmt.Errorf("bigcore: %s: restoring state: %w (core left halted)", th.name, err)
		}
	}
	if _, err := t.s.IRDR(c.tap, irProbeMode, 4, big.NewInt(0)); err != nil {
		return err
	}
	armed := false
	for _, th := range c.threads {
		th.prepared, th.dr6Set = false, false
		armed = armed || th.Armed() || th.stepping
	}
	c.threads = map[int]*Thread{}
	c.halted = false
	if c.wakeOld != nil {
		wake, _ := c.uncore.Register("pcode_wake_req")
		if _, err := t.s.Exchange(c.uncore, wake, c.wakeOld); err != nil {
			return err
		}
		c.wakeOld = nil
	}
	t.trigger(false)
	if armed {
		return nil // may stop again right away; Wait reports it
	}
	deadline := time.Now().Add(time.Second)
	for {
		h, err := t.Halted()
		if err != nil {
			return err
		}
		if !h {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("bigcore: %s did not leave probe mode", t.name)
		}
		time.Sleep(time.Millisecond)
	}
}

// pc returns the linear address of the next instruction (CS base + RIP).
func (t *Thread) pc() uint64 {
	base, err := t.ReadRegister("csbase")
	if err != nil {
		return t.scp[7]
	}
	return base + t.scp[7]
}

// ---- registers ----

var gpr = map[string]int{"rbx": rBX, "rsi": rSI, "rdi": rDI, "rbp": rBP, "rsp": rSP,
	"r8": r8, "r9": r9, "r10": r10, "r11": r11, "r12": r12, "r13": r13, "r14": r14, "r15": r15}

var regOrder = []string{
	"rax", "rbx", "rcx", "rdx", "rsi", "rdi", "rbp", "rsp",
	"r8", "r9", "r10", "r11", "r12", "r13", "r14", "r15",
	"rip", "rflags", "cs", "cpl", "cr0", "cr2", "cr3", "cr4", "cr8",
	"dr0", "dr1", "dr2", "dr3", "dr6", "dr7", "efer",
	"csbase", "dsbase", "esbase", "ssbase", "fsbase", "gsbase", "gdtbase", "idtbase",
}

var only64 = map[string]bool{"r8": true, "r9": true, "r10": true, "r11": true, "r12": true,
	"r13": true, "r14": true, "r15": true, "cr8": true}

func (t *Thread) Registers() []string {
	if t.core.halted && !t.prepared {
		t.prepare() // the register set depends on the thread's mode
	}
	if t.prepared && t.mode != 64 {
		var out []string
		for _, r := range regOrder {
			if !only64[r] {
				out = append(out, r)
			}
		}
		return out
	}
	return regOrder
}

func (t *Thread) need() error {
	if !t.core.halted {
		return fmt.Errorf("bigcore: %s is not halted", t.name)
	}
	return t.prepare()
}

func (t *Thread) ReadRegister(name string) (uint64, error) {
	if err := t.need(); err != nil {
		return 0, err
	}
	switch name {
	case "rax":
		return t.scp[8], nil
	case "rcx":
		return t.scp[1], nil
	case "rdx":
		return t.scp[3], nil
	case "rip":
		return t.scp[7], nil
	case "rbx":
		return t.rbx, nil
	case "rflags":
		return t.rflags, nil
	case "cpl":
		return t.cpl, nil
	case "dr6":
		return t.dr6, nil
	case "dr7":
		return t.dr7, nil
	case "cs":
		return t.entry >> 32 & 0xffff, nil
	case "efer":
		return t.ReadMSR(msrEFER)
	case "cr0":
		return t.scp[9], nil // saved by the microcode, reloaded on exit
	}
	if f := t.flat; f != nil {
		switch name {
		case "rsi":
			return f.rsi, nil
		case "rdi":
			return f.rdi, nil
		case "cr3":
			return f.cr3, nil
		case "cr4":
			return f.cr4, nil
		}
		for seg, n := range map[string]int{"csbase": segCS, "ssbase": segSS, "gdtbase": segGDT, "idtbase": segIDT} {
			if name == seg {
				return f.seg[n][0], nil
			}
		}
	}
	if r, ok := gpr[name]; ok {
		if r >= 8 && t.mode != 64 {
			return 0, fmt.Errorf("bigcore: %s only exists in 64-bit mode", name)
		}
		return t.one(func(p *prog, e enc) { p.exec(e.movRR(rAX, r), e.movR32Imm(rCX, msrProbeGPReg), e.rdmsr()) })
	}
	var n int
	if _, err := fmt.Sscanf(name, "cr%d", &n); err == nil && (n <= 4 || n == 8) && n != 1 {
		if n == 8 && t.mode != 64 {
			return 0, errors.New("bigcore: cr8 only exists in 64-bit mode")
		}
		return t.one(func(p *prog, e enc) { p.exec(e.movFromCR(rAX, n), e.movR32Imm(rCX, msrProbeGPReg), e.rdmsr()) })
	}
	if _, err := fmt.Sscanf(name, "dr%d", &n); err == nil && n <= 3 {
		return t.one(func(p *prog, e enc) { p.exec(e.movFromDR(rAX, n), e.movR32Imm(rCX, msrProbeGPReg), e.rdmsr()) })
	}
	if len(name) > 4 && name[len(name)-4:] == "base" {
		seg, ok := segNum[name[:len(name)-4]]
		if ok {
			return t.one(func(p *prog, e enc) {
				p.exec(e.movR32Imm(rCX, msrProbeSeg), e.movR32Imm(rAX, uint32(seg)), e.rdmsr())
			})
		}
	}
	return 0, fmt.Errorf("bigcore: unknown register %q", name)
}

func (t *Thread) ReadRegisters(names []string) ([]uint64, error) {
	out := make([]uint64, len(names))
	for i, n := range names {
		v, err := t.ReadRegister(n)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", n, err)
		}
		out[i] = v
	}
	return out, nil
}

// ---- MSRs ----

func (t *Thread) readMSR(index uint32) (uint64, error) {
	if err := t.need(); err != nil {
		return 0, err
	}
	if f := t.flat; f != nil {
		switch index {
		case msrEFER:
			return f.efer, nil
		case msrEPTP:
			return f.eptp, nil
		}
	}
	v, err := t.run(func(p *prog, e enc) {
		// clear the fault flag, read, then low half via EAX and high half
		// via SCP1, then the fault flag
		p.exec(e.movR32Imm(rCX, msrICECTLPMR), e.rdmsr(), e.andEAXImm(^uint32(iceFault)), e.wrmsr())
		p.exec(e.movR32Imm(rCX, index), e.rdmsr(), e.movR32Imm(rCX, msrProbeGPReg), e.rdmsr())
		p.pdr()
		p.exec(e.movR32Imm(rAX, msrSCP0+1), e.movR32Imm(rCX, msrProbeToPDR), e.rdmsr())
		p.pdr()
		p.exec(e.movR32Imm(rCX, msrICECTLPMR), e.rdmsr(), e.andEAXImm(iceFault), e.shrImm(rAX, 4),
			e.movR32Imm(rCX, msrProbeGPReg), e.rdmsr())
		p.pdr()
	})
	if err != nil {
		return 0, err
	}
	if v[2]&1 != 0 {
		return 0, fmt.Errorf("bigcore: rdmsr %#x faulted", index)
	}
	return v[1]<<32 | v[0]&0xffffffff, nil
}

// ReadMSR reads an MSR. A faulting RDMSR is caught by the core in probe
// mode and reported as an error.
func (t *Thread) ReadMSR(index uint32) (uint64, error)      { return t.readMSR(index) }
func (t *Thread) ReadMSRForce(index uint32) (uint64, error) { return t.readMSR(index) }

// ---- writes ----

// WriteRegister changes the program-visible value. Registers that the
// microcode or the debugger saves are updated in the saved copy and take
// effect on resume; the others are written directly.
func (t *Thread) WriteRegister(name string, v uint64) error {
	if err := t.need(); err != nil {
		return err
	}
	switch name {
	case "rax":
		t.scp[8] = v
	case "rcx":
		t.scp[1] = v
	case "rdx":
		t.scp[3] = v
	case "rip":
		t.scp[7] = v
	case "cr0":
		t.scp[9] = v
	case "rbx":
		t.rbx = v
	case "rflags":
		t.rflags = v
	case "cpl":
		t.cpl = v
	case "dr6":
		t.dr6, t.dr6Set = v, true
	case "dr7":
		t.dr7 = v
	default:
		return t.writeLive(name, v)
	}
	return nil
}

func (t *Thread) writeLive(name string, v uint64) error {
	if f := t.flat; f != nil {
		switch name {
		case "rsi":
			f.rsi = v
			return nil
		case "rdi":
			f.rdi = v
			return nil
		case "cr3":
			f.cr3 = v
			return nil
		case "cr4":
			f.cr4 = v
			return nil
		}
	}
	if r, ok := gpr[name]; ok {
		if r >= 8 && t.mode != 64 {
			return fmt.Errorf("bigcore: %s only exists in 64-bit mode", name)
		}
		_, err := t.run(func(p *prog, e enc) { p.exec(e.movImm(r, v)) })
		return err
	}
	var n int
	if _, err := fmt.Sscanf(name, "cr%d", &n); err == nil && (n == 2 || n == 3 || n == 4 || n == 8) {
		_, err := t.run(func(p *prog, e enc) { p.exec(e.movImm(rAX, v), e.movToCR(n, rAX)) })
		return err
	}
	if _, err := fmt.Sscanf(name, "dr%d", &n); err == nil && n <= 3 {
		_, err := t.run(func(p *prog, e enc) { p.exec(e.movImm(rAX, v), e.movToDR(n, rAX)) })
		return err
	}
	return fmt.Errorf("bigcore: register %q cannot be written", name)
}

func (t *Thread) writeMSR(index uint32, v uint64) error {
	if err := t.need(); err != nil {
		return err
	}
	switch {
	case index == msrICECTLPMR:
		t.icectl = v
		return nil
	case t.flat != nil && index == msrEFER:
		t.flat.efer = v
		return nil
	case t.flat != nil && index == msrEPTP:
		t.flat.eptp = v
		return nil
	}
	out, err := t.run(func(p *prog, e enc) {
		clearFault(p, e)
		p.exec(e.movR32Imm(rCX, index), e.movR32Imm(rAX, uint32(v)), e.movR32Imm(rDX, uint32(v>>32)), e.wrmsr())
		faultCheck(p, e)
	})
	if err != nil {
		return err
	}
	if out[0]&1 != 0 {
		return fmt.Errorf("bigcore: wrmsr %#x faulted", index)
	}
	return nil
}

// WriteMSR writes an MSR; a faulting WRMSR is caught and reported.
func (t *Thread) WriteMSR(index uint32, v uint64) error      { return t.writeMSR(index, v) }
func (t *Thread) WriteMSRForce(index uint32, v uint64) error { return t.writeMSR(index, v) }

// ---- breakpoints and single-step ----

const (
	eflTF = 1 << 8
	eflRF = 1 << 16
	iceIR = 1 << 0 // redirect debug exceptions (#DB) into probe mode
)

type breakpoint struct {
	addr   uint64
	rw     uint64
	length int
}

func (bp breakpoint) dr7Bits(i int) uint64 {
	ln := map[int]uint64{1: 0, 2: 1, 8: 2, 4: 3}[bp.length]
	if bp.rw == 0 {
		ln = 0
	}
	return 1<<(2*i) | (bp.rw|ln<<2)<<(16+4*i)
}

func (t *Thread) SetBreakpoint(slot int, addr uint64, kind string, length int) error {
	rw, ok := map[string]uint64{"x": 0, "w": 1, "io": 2, "rw": 3}[kind]
	if !ok {
		return errors.New("bigcore: breakpoint kind must be x, w, rw or io")
	}
	if slot < 0 || slot > 3 {
		return errors.New("bigcore: breakpoint slot must be 0..3")
	}
	if err := t.need(); err != nil { // breakpoints are applied when resuming
		return err
	}
	if rw == 0 {
		length = 1
	} else if length != 1 && length != 2 && length != 4 && length != 8 || addr%uint64(length) != 0 {
		return errors.New("bigcore: data breakpoints need an aligned length of 1, 2, 4 or 8")
	}
	t.bps[slot] = &breakpoint{addr, rw, length}
	t.usedSlot[slot] = true
	return nil
}

func (t *Thread) ClearBreakpoint(slot int) error {
	if slot < 0 || slot > 3 {
		return errors.New("bigcore: breakpoint slot must be 0..3")
	}
	t.bps[slot] = nil
	return nil
}

// Armed reports whether breakpoints are set.
func (t *Thread) Armed() bool {
	for _, bp := range t.bps {
		if bp != nil {
			return true
		}
	}
	return false
}

// debugOverlay returns the DR7, ICECTLPMR and RFLAGS values to resume with.
func (t *Thread) debugOverlay() (dr7, ice, rflags uint64, err error) {
	// Never leave debugger-only settings behind: the program doesn't use
	// the redirect/guard bits, and slots we used get their enables cleared.
	dr7, ice, rflags = t.dr7, t.icectl&^(iceRemInPM|iceIR|icePDR), t.rflags
	for i := 0; i < 4; i++ {
		if t.usedSlot[i] {
			dr7 &^= 3 << (2 * i)
		}
	}
	var bits uint64
	for i, bp := range t.bps {
		if bp != nil {
			bits |= bp.dr7Bits(i)
		}
	}
	if bits != 0 || t.stepping {
		dr7 |= bits
		ice |= iceIR
		rflags |= eflRF
	}
	if bits != 0 {
		ice |= icePDR // guard the debug registers while our breakpoints are set
	}
	if t.stepping {
		rflags |= eflTF
	}
	if bits != 0 {
		// The debug registers are live: program the addresses now.
		_, err = t.run(func(p *prog, e enc) {
			for i, bp := range t.bps {
				if bp != nil {
					p.exec(e.movImm(rAX, bp.addr), e.movToDR(i, rAX))
				}
			}
		})
	}
	return
}

// Step executes one instruction of this thread (its sibling runs freely
// meanwhile) and stops again.
func (t *Thread) Step() (runctl.StopInfo, error) {
	if err := t.need(); err != nil {
		return runctl.StopInfo{}, err
	}
	t.stepping = true
	if err := t.Resume(); err != nil {
		t.stepping = false
		return runctl.StopInfo{}, err
	}
	si, stopped, err := t.Wait(time.Second)
	if err != nil {
		return si, err
	}
	if !stopped {
		if _, herr := t.Halt(); herr != nil {
			return si, fmt.Errorf("bigcore: %s did not stop after a step, and halting failed: %v", t.name, herr)
		}
		return si, fmt.Errorf("bigcore: %s did not stop after a step; halted it", t.name)
	}
	return si, nil
}

// Wait polls until the thread re-enters probe mode (breakpoint, step) or
// timeout expires.
func (t *Thread) Wait(timeout time.Duration) (runctl.StopInfo, bool, error) {
	if t.core.halted {
		return runctl.StopInfo{PC: t.scp[7], Reason: "already halted"}, true, nil
	}
	deadline := time.Now().Add(timeout)
	for {
		st, err := t.tapStatus()
		if err != nil {
			return runctl.StopInfo{}, false, err
		}
		if t.inPM(st) {
			si, err := t.stopped()
			return si, err == nil, err
		}
		if time.Now().After(deadline) {
			return runctl.StopInfo{}, false, nil
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// wholeCore makes sure every thread of the core is in probe mode: the
// core only signals probe-mode ready (the trigger used to wait for
// instruction completion) once all of its threads are.
func (t *Thread) wholeCore() error {
	st, err := t.tapStatus()
	if err != nil {
		return err
	}
	all := func(st uint64) bool {
		return st&(tsPMRdyT0|tsDisabledT0) != 0 && st&(tsPMRdyT1|tsDisabledT1) != 0
	}
	if all(st) {
		return nil
	}
	if _, err := t.s.IRDR(t.core.tap, irProbeMode, 4, big.NewInt(2)); err != nil {
		return err
	}
	deadline := time.Now().Add(time.Second)
	for {
		if st, err = t.tapStatus(); err != nil {
			return err
		}
		if all(st) {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("bigcore: core %d: sibling thread did not enter probe mode (tap_status %#x)", t.core.id, st)
		}
		time.Sleep(time.Millisecond)
	}
}

// stopped takes over a core that entered probe mode by itself.
func (t *Thread) stopped() (runctl.StopInfo, error) {
	if err := t.wholeCore(); err != nil {
		return runctl.StopInfo{}, err
	}
	t.core.halted = true
	stepping := t.stepping
	if err := t.prepare(); err != nil {
		return runctl.StopInfo{}, err
	}
	reason := "debug exception"
	switch {
	case t.dr6&0xf != 0:
		for i := 0; i < 4; i++ {
			if t.dr6&(1<<i) != 0 && t.bps[i] != nil {
				reason = fmt.Sprintf("breakpoint %d", i)
			}
		}
	case t.dr6&(1<<14) != 0 && stepping:
		reason = "single step"
	default:
		reason = fmt.Sprintf("probe-mode entry (prior state %#x)", t.entry>>16&0xff)
	}
	// Hide what the debugger caused.
	if stepping {
		t.rflags &^= eflTF
		t.stepping = false
	}
	t.rflags &^= eflRF
	t.dr7, t.icectl = t.progDR7, t.progICE
	return runctl.StopInfo{PC: t.pc(), Reason: reason}, nil
}
