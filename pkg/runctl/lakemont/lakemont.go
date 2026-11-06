// SPDX-License-Identifier: GPL-2.0-only

// Package lakemont implements run control for Lakemont-class x86 cores
// (Quark, and the CSME core in Intel PCHs) through their TAP's probe mode.
//
// Probe mode in a nutshell: the TAP has a probe-mode request register; once
// the core is in probe mode, the host writes 64-bit "probe instructions"
// (PIR), and executes each by passing through Update-IR with the SUBMIT
// opcode. Data moves through a 32-bit probe data register (PDR). Register
// values live in a shadow RAM: selecting a register and issuing an SRAM
// access makes it the target of "SRAM -> PDR" (read) or "PDR -> SRAM"
// (write). Memory and I/O are accessed by putting operands in EAX/EDX and
// submitting a memory or I/O probe instruction.
package lakemont

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math/big"
	"math/bits"
	"os"
	"path/filepath"
	"slices"
	"time"

	"libredci/pkg/model"
	"libredci/pkg/runctl"
	"libredci/pkg/tapnet"
)

// TAP instructions and data register sizes.
const (
	irSubmit    = 0x03 // IR-only: execute the PIR
	irProbeMode = 0x04 // 1 bit: request (1) / leave (0) probe mode
	irWritePIR  = 0x06 // 64 bits
	irPDR       = 0x08 // 32 bits, read/write
	irStatus    = 0x0b // 32 bits
	irResetBrk  = 0x35 // 1 bit: halt at the reset vector on the next reset

	pirBits = 64
	pdrBits = 32
)

// Status register bits.
const (
	stPRDY     = 1 << 0  // PRDY signal
	stPMReq    = 1 << 1  // probe-mode request pending
	stInPM     = 1 << 2  // in probe mode
	stRedirect = 1 << 3  // probe-mode redirection
	stSWBP     = 1 << 4  // software breakpoints supported
	stMemBusy  = 1 << 9  // memory operation in progress
	stResetBrk = 1 << 18 // stopped by the TAP-based reset break
)

// Debug register bits.
const (
	dr6BS    = 1 << 14 // single step
	dr6Bmask = 0xf     // B0..B3
	eflTF    = 1 << 8
	eflRF    = 1 << 16
	pmcrIR   = 1 << 0 // redirect #DB (int1) into probe mode
)

// Probe instructions.
const (
	pirSRAMAccess = 0x0000000e9d660000
	pirSRAMToPDR  = 0x4cf0000000000000
	pirPDRToSRAM  = 0x0cf0000000000000
	pirWBINVD     = 0x09090909090990f0

	pirMemRd8, pirMemRd16, pirMemRd32 = 0x0909090909090851, 0x090909090908d166, 0x09090909090908d1
	pirMemWr8, pirMemWr16, pirMemWr32 = 0x0909090909090811, 0x0909090909089166, 0x0909090909090891
	pirIORd8, pirIORd16, pirIORd32    = 0x0909090909090937, 0x090909090909b766, 0x09090909090909b7
	pirIOWr8, pirIOWr16, pirIOWr32    = 0x0909090909090977, 0x090909090909f766, 0x09090909090909f7
)

// PIR encodes up to 8 bytes of x86 machine code as a probe instruction:
// each byte is bit-reversed and they are packed low byte first, padded
// with NOPs. (E.g. RDMSR 0f 32 -> 0x0909090909094cf0, matching the fixed
// encodings above: WBINVD 0f 09, "mov edx,[eax]" 8b 10, "in al,dx" ec.)
func PIR(code ...byte) uint64 {
	if len(code) > 8 {
		panic("lakemont: probe instruction longer than 8 bytes")
	}
	var v uint64
	for i := 0; i < 8; i++ {
		b := byte(0x90) // nop
		if i < len(code) {
			b = code[i]
		}
		v |= uint64(bits.Reverse8(b)) << (8 * i)
	}
	return v
}

var (
	pirRDMSR = PIR(0x0f, 0x32)
	pirWRMSR = PIR(0x0f, 0x30)
)

// Register select probe instructions (32-bit protected-mode encodings).
var regSelect = map[string]uint64{
	"eax": 0xd01d660000, "ecx": 0x501d660000, "edx": 0x901d660000, "ebx": 0x101d660000,
	"esp": 0xe01d660000, "ebp": 0x601d660000, "esi": 0xa01d660000, "edi": 0x201d660000,
	"eip": 0xc01d660000, "eflags": 0x401d660000,
	"cs": 0x281d660000, "ss": 0xc81d660000, "ds": 0x481d660000, "es": 0xa81d660000,
	"fs": 0x881d660000, "gs": 0x081d660000,
	"cr0": 0x001d660000, "cr2": 0xbc1d660000, "cr3": 0x801d660000, "cr4": 0x2c1d660000,
	"dr0": 0x7c1d660000, "dr1": 0xfc1d660000, "dr2": 0x021d660000, "dr3": 0x821d660000,
	"dr6": 0x301d660000, "dr7": 0xb01d660000,
	"idtbase": 0x581d660000, "idtlimit": 0xd81d660000, "idtar": 0x981d660000,
	"gdtbase": 0xb81d660000, "gdtlimit": 0x781d660000, "gdtar": 0x381d660000,
	"tr": 0x701d660000, "ldtr": 0xf01d660000,
	"ldtbase": 0x041d660000, "ldtlimit": 0x841d660000, "ldtar": 0xf81d660000,
	"csbase": 0xf41d660000, "cslimit": 0x0c1d660000, "csar": 0x741d660000,
	"dsbase": 0x941d660000, "dslimit": 0x541d660000, "dsar": 0x141d660000,
	"esbase": 0x4c1d660000, "eslimit": 0xcc1d660000, "esar": 0x8c1d660000,
	"fsbase": 0x641d660000, "fslimit": 0xe41d660000, "fsar": 0xa41d660000,
	"gsbase": 0xc41d660000, "gslimit": 0x241d660000, "gsar": 0x441d660000,
	"ssbase": 0x341d660000, "sslimit": 0xb41d660000, "ssar": 0xd41d660000,
	"tssbase": 0xe81d660000, "tsslimit": 0x181d660000, "tssar": 0x681d660000,
	"pmcr": 0x421d660000,
}

var regOrder = []string{
	"eax", "ebx", "ecx", "edx", "esi", "edi", "ebp", "esp", "eip", "eflags",
	"cs", "ds", "es", "fs", "gs", "ss",
	"cr0", "cr2", "cr3", "cr4", "dr0", "dr1", "dr2", "dr3", "dr6", "dr7",
	"gdtbase", "gdtlimit", "gdtar", "idtbase", "idtlimit", "idtar",
	"ldtr", "ldtbase", "ldtlimit", "ldtar", "tr", "tssbase", "tsslimit", "tssar",
	"csbase", "cslimit", "csar", "dsbase", "dslimit", "dsar", "esbase", "eslimit", "esar",
	"fsbase", "fslimit", "fsar", "gsbase", "gslimit", "gsar", "ssbase", "sslimit", "ssar",
	"pmcr",
}

// Values used to set up a flat ring-0 32-bit environment for debug accesses.
const (
	flatAR = 0x004f9300 // present, writable data / 32-bit, DPL 0, 4K granular
	// Attribute bits 13:8 (limit 19:16, AVL, L) sit at bits 21:16 of the
	// CS/SS attribute registers; OpenIPC saves the program values with them
	// clear.
	arUpper = 0x003f0000
	eflIF   = 1 << 9
	eflVM   = 1 << 17
	cr0PG   = 1 << 31
	cr0CD   = 1 << 30
	cr0NW   = 1 << 29
)

func init() {
	runctl.Register("lakemont", func(s *tapnet.Session, tap *model.TAP, th model.Thread) (runctl.Thread, error) {
		c := &Core{s: s, tap: tap, name: th.Name}
		// A previous session may have left breakpoints armed while the core
		// ran; pick up the program's DR7/PMCR so they can be restored.
		if st, err := c.loadState(); err == nil && st.Orig == nil {
			c.progDR7, c.progPMCR, c.stepping, c.progDR = st.ProgDR7, st.ProgPMCR, st.Stepping, st.ProgDR
			c.owned = st.ProgDR7 != nil
		}
		return c, nil
	})
}

// Core is a Lakemont core.
type Core struct {
	s    *tapnet.Session
	tap  *model.TAP
	name string

	halted   bool
	bps      [4]*breakpoint
	stepping bool
	// progDR7/progPMCR are the program's own values while the debugger
	// uses DR7/PMCR for breakpoints or stepping (nil when it doesn't).
	progDR7, progPMCR *uint32
	// progDR holds the program's DR0-DR3 for slots the debugger has used;
	// a slot gets its value back once its breakpoint is cleared.
	progDR map[int]uint32

	// owned: this session halted the core and saved its pre-halt state, so
	// Resume can restore it. prepared: the debug environment is set up.
	owned, prepared bool
	// orig holds the program's values of registers the debugger had to
	// change (halt prep, EAX/EDX for memory access); they are restored on
	// resume and shown instead of the scratch values.
	orig map[string]uint32
}

func (c *Core) Name() string        { return c.name }
func (c *Core) Registers() []string { return regOrder }

func (c *Core) status() (uint32, error) {
	v, err := c.s.IRDR(c.tap, irStatus, 32, nil)
	if err != nil {
		return 0, err
	}
	return uint32(v.Uint64()), nil
}

func (c *Core) Halted() (bool, error) {
	st, err := c.status()
	return st&stInPM != 0, err
}

// ---- batched probe-mode operations ----

// batch accumulates probe operations for one JTAG transaction.
type batch struct {
	o     *tapnet.Ops
	reads []*tapnet.Result
	st    *tapnet.Result
}

func (b *batch) submit(pir uint64) {
	b.o.IRDR(irWritePIR, pirBits, new(big.Int).SetUint64(pir))
	b.o.IR(irSubmit)
	b.o.Idle(2)
}

// settleTCKs is how long to idle after a memory or I/O probe instruction
// before touching the probe registers again (~1 ms at the PCH chain's TCK,
// the same as the reference tool). Issuing the next probe instruction
// while the access is still in flight wedges the core.
const settleTCKs = 3750

// submitBus submits a memory or I/O probe instruction and waits for it.
func (b *batch) submitBus(pir uint64) {
	b.submit(pir)
	b.o.Idle(settleTCKs)
}

// readReg queues a register read; its index in b.reads is returned.
func (b *batch) readReg(name string) int {
	b.submit(regSelect[name])
	b.submit(pirSRAMAccess)
	b.submit(pirSRAMToPDR)
	b.reads = append(b.reads, b.o.IRDR(irPDR, pdrBits, nil))
	return len(b.reads) - 1
}

func (b *batch) writeReg(name string, v uint32) {
	b.submit(regSelect[name])
	b.submit(pirSRAMAccess)
	b.o.IRDR(irPDR, pdrBits, new(big.Int).SetUint64(uint64(v)))
	b.submit(pirPDRToSRAM)
}

// checkStatus queues a status read to verify the preceding operations.
func (b *batch) checkStatus() { b.st = b.o.IRDR(irStatus, 32, nil) }

func (c *Core) run(fn func(b *batch)) ([]uint32, error) {
	var b *batch
	err := c.s.Do(c.tap, func(o *tapnet.Ops) {
		b = &batch{o: o}
		fn(b)
	})
	if err != nil {
		return nil, err
	}
	out := make([]uint32, len(b.reads))
	for i, r := range b.reads {
		out[i] = uint32(r.Uint64())
	}
	if b.st != nil {
		st := uint32(b.st.Uint64())
		// Long-running probe instructions leave the core busy for a while;
		// wait for it to settle back into plain probe mode.
		for i := 0; st&(stPMReq|stMemBusy) != 0 || st&stInPM == 0; i++ {
			if i == 50 {
				return out, fmt.Errorf("lakemont: %s: probe transaction failed (status %#x)", c.name, st)
			}
			c.s.Wait(time.Millisecond)
			if st, err = c.status(); err != nil {
				return out, err
			}
		}
	}
	return out, nil
}

func (c *Core) needHalted() error {
	if !c.halted {
		return fmt.Errorf("lakemont: %s is not halted (by us)", c.name)
	}
	return nil
}

// ---- halt / resume ----

func (c *Core) Halt() (runctl.StopInfo, error) {
	if c.halted {
		return c.stopInfo("already halted")
	}
	st, err := c.status()
	if err != nil {
		return runctl.StopInfo{}, err
	}
	if st&stInPM != 0 && c.owned {
		// We resumed it with breakpoints/stepping armed and it stopped.
		return c.entered()
	}
	if st&stInPM == 0 && c.progDR7 == nil {
		// Running and not armed by us: any state file is stale (e.g. the
		// platform reset under a previous session).
		c.forget()
	}
	if st&stInPM != 0 {
		return runctl.StopInfo{}, fmt.Errorf("lakemont: %s is already in probe mode (halted by someone else or an earlier session); use attach", c.name)
	}
	{
		if _, err := c.s.IRDR(c.tap, irProbeMode, 1, big.NewInt(1)); err != nil {
			return runctl.StopInfo{}, err
		}
		for i := 0; ; i++ {
			if st, err = c.status(); err != nil {
				return runctl.StopInfo{}, err
			}
			if st&stInPM != 0 && st&stPMReq == 0 {
				break
			}
			if i == 100 {
				return runctl.StopInfo{}, fmt.Errorf("lakemont: %s did not enter probe mode (status %#x)", c.name, st)
			}
		}
	}
	return c.entered()
}

// entered takes over a core that has just entered probe mode after running
// under our control (halt request, breakpoint or single step): it saves the
// program state, sets up the debug environment and reports why it stopped.
func (c *Core) entered() (runctl.StopInfo, error) {
	c.halted, c.owned = true, true
	c.orig = map[string]uint32{}
	// Why did it stop? Read before prepare touches anything.
	dr6, err := c.readRaw("dr6")
	if err != nil {
		return runctl.StopInfo{}, err
	}
	if err := c.prepare(); err != nil {
		// Put back whatever was changed and stay halted: resuming with a
		// half-prepared context would crash the firmware.
		if rerr := c.restore(); rerr != nil {
			c.owned = false
			return runctl.StopInfo{}, fmt.Errorf("lakemont: %s: debug setup failed (%v) and restoring failed (%v); left halted, do not resume", c.name, err, rerr)
		}
		c.orig = map[string]uint32{}
		return runctl.StopInfo{}, fmt.Errorf("lakemont: %s: debug setup failed, original state restored, left halted: %w", c.name, err)
	}
	reason := "halt request"
	if st, err := c.status(); err == nil && st&stResetBrk != 0 {
		reason = "reset break"
	}
	switch {
	case dr6&dr6Bmask != 0:
		for i := 0; i < 4; i++ {
			if dr6&(1<<i) != 0 && c.bps[i] != nil {
				reason = fmt.Sprintf("breakpoint %d", i)
				break
			}
		}
		if reason == "halt request" {
			reason = fmt.Sprintf("debug exception (dr6 %#x)", dr6)
		}
	case dr6&dr6BS != 0:
		reason = "single step"
	}
	// Hide what the debugger caused from the program: our DR6 status bits
	// and the trap flag used for stepping.
	if dr6&(dr6Bmask|dr6BS) != 0 && c.progDR7 != nil {
		c.orig["dr6"] = dr6 &^ (dr6Bmask | dr6BS)
	}
	if c.stepping {
		c.orig["eflags"] &^= eflTF
		c.stepping = false
	}
	if err := c.persist(); err != nil {
		return runctl.StopInfo{}, err
	}
	return c.stopInfo(reason)
}

func (c *Core) readRaw(name string) (uint32, error) {
	v, err := c.run(func(b *batch) { b.readReg(name) })
	if err != nil {
		return 0, err
	}
	return v[0], nil
}

// prepare makes memory accesses work regardless of what the program was
// doing: flat 4 GiB ring-0 CS/DS/SS, no paging, interrupts off, caches
// flushed and disabled, hardware breakpoints off. Everything it changes is
// saved in c.orig.
func (c *Core) prepare() error {
	save := []string{"eflags", "csbase", "cslimit", "csar", "ssar", "dsbase", "dslimit", "dsar", "cr0", "dr7"}
	v, err := c.run(func(b *batch) {
		for _, r := range save {
			b.readReg(r)
		}
	})
	if err != nil {
		return err
	}
	cur := map[string]uint32{}
	for i, r := range save {
		cur[r] = v[i]
		c.orig[r] = v[i]
	}
	c.orig["csar"] &^= arUpper
	c.orig["ssar"] &^= arUpper
	if c.progDR7 != nil {
		// DR7 currently holds our breakpoints; the program's is remembered.
		c.orig["dr7"] = *c.progDR7
	}
	if err := c.persist(); err != nil {
		return fmt.Errorf("saving state file: %w", err)
	}
	c.prepared = true
	// OpenIPC's sequence (save_minimum_state), in its order.
	efl, cr0 := cur["eflags"], cur["cr0"]
	ssDPL := cur["ssar"] >> 13 & 3
	_, err = c.run(func(b *batch) {
		b.writeReg("dsbase", 0)
		b.writeReg("dslimit", 0xffffffff)
		b.writeReg("dsar", flatAR)
		b.writeReg("dr7", 0)
		b.writeReg("csbase", 0)
		b.writeReg("cslimit", 0xffffffff)
		b.writeReg("csar", flatAR)
		if efl&eflVM != 0 {
			efl &^= eflVM
			b.writeReg("eflags", efl)
		}
		if ssDPL != 0 {
			b.writeReg("ssar", flatAR)
		}
		if efl&eflIF != 0 {
			efl &^= eflIF
			b.writeReg("eflags", efl)
		}
		if cr0&cr0PG != 0 {
			cr0 &^= cr0PG
			b.writeReg("cr0", cr0)
		}
		if cr0&cr0CD == 0 {
			// OpenIPC also issues WBINVD here when CS.DPL is 0. It is not
			// issued: on SPT's CSME a WBINVD probe instruction was observed
			// to leave the core busy with an odd status (not re-tested).
			cr0 |= cr0CD | cr0NW
			b.writeReg("cr0", cr0)
		}
		b.checkStatus()
	})
	return err
}

func (c *Core) stopInfo(reason string) (runctl.StopInfo, error) {
	r, err := c.ReadRegisters([]string{"eip", "csbase"})
	if err != nil {
		return runctl.StopInfo{}, err
	}
	return runctl.StopInfo{PC: uint64(uint32(r[0] + r[1])), Reason: reason}, nil
}

func (c *Core) Resume() error { return c.resume(false) }

// Step executes one instruction and returns once the core is back in
// probe mode. (If an interrupt is pending, that "one instruction" is the
// first one of the handler.)
func (c *Core) Step() (runctl.StopInfo, error) {
	if err := c.resume(true); err != nil {
		return runctl.StopInfo{}, err
	}
	si, stopped, err := c.Wait(time.Second)
	if err != nil {
		return si, err
	}
	if !stopped {
		// Never leave it running with TF set: halt it (entered() strips TF).
		if _, herr := c.Halt(); herr != nil {
			return si, fmt.Errorf("lakemont: %s did not stop after a single step, and halting failed: %v", c.name, herr)
		}
		return si, fmt.Errorf("lakemont: %s did not stop after a single step; halted it instead", c.name)
	}
	return si, nil
}

func (c *Core) resume(step bool) error {
	if err := c.needHalted(); err != nil {
		return err
	}
	if !c.owned {
		return fmt.Errorf("lakemont: %s: pre-halt state unknown; refusing to resume (use ForceResume if you restored it yourself)", c.name)
	}
	if err := c.restore(); err != nil {
		return err
	}
	if err := c.applyDebug(step); err != nil {
		return err
	}
	// With breakpoints or stepping armed the core may stop again right away,
	// so only a plain resume is checked for having left probe mode.
	return c.exitProbeMode(!step && !c.Armed())
}

// applyDebug programs breakpoints and stepping on top of the restored
// program state (or puts the program's own DR7/PMCR back when no longer
// needed).
func (c *Core) applyDebug(step bool) error {
	var bits uint32
	for i, bp := range c.bps {
		if bp != nil {
			bits |= bp.dr7Bits(i)
		}
	}
	active := bits != 0 || step
	if !active && c.progDR7 == nil && len(c.progDR) == 0 {
		return nil // never touched
	}
	// Keep the program's DRn before the first breakpoint in that slot.
	var need []int
	for i, bp := range c.bps {
		if _, ok := c.progDR[i]; bp != nil && !ok {
			need = append(need, i)
		}
	}
	if len(need) > 0 {
		v, err := c.run(func(b *batch) {
			for _, i := range need {
				b.readReg(fmt.Sprintf("dr%d", i))
			}
		})
		if err != nil {
			return err
		}
		if c.progDR == nil {
			c.progDR = map[int]uint32{}
		}
		for j, i := range need {
			c.progDR[i] = v[j]
		}
	}
	// Slots no longer used get the program's value back.
	free := map[int]uint32{}
	for i, v := range c.progDR {
		if c.bps[i] == nil {
			free[i] = v
			delete(c.progDR, i)
		}
	}
	if active && c.progDR7 == nil {
		v, err := c.run(func(b *batch) { b.readReg("pmcr") })
		if err != nil {
			return err
		}
		dr7, pmcr := c.orig["dr7"], v[0]
		c.progDR7, c.progPMCR = &dr7, &pmcr
		if err := c.persist(); err != nil {
			return err
		}
	}
	_, err := c.run(func(b *batch) {
		for i, v := range free {
			b.writeReg(fmt.Sprintf("dr%d", i), v)
		}
		if !active {
			if c.progPMCR != nil {
				b.writeReg("pmcr", *c.progPMCR)
			}
			return // dr7 was restored from orig already
		}
		for i, bp := range c.bps {
			if bp != nil {
				b.writeReg(fmt.Sprintf("dr%d", i), bp.addr)
			}
		}
		b.writeReg("dr7", *c.progDR7|bits)
		b.writeReg("pmcr", *c.progPMCR|pmcrIR)
		efl := c.orig["eflags"] | eflRF // don't re-trigger at the current address
		if step {
			efl |= eflTF
		}
		b.writeReg("eflags", efl)
		b.checkStatus()
	})
	if err != nil {
		return err
	}
	c.stepping = step
	if err := c.persist(); err != nil {
		return err
	}
	if !active {
		c.progDR7, c.progPMCR = nil, nil
	}
	return nil
}

// SetResetBreak arms (or disarms) the TAP-based reset break: on its next
// reset the core stops in probe mode at the reset vector instead of
// running. Nothing needs restoring after such a stop (the state is the
// reset state), so it is taken over like a normal halt. Note that on SPT
// the CSME only resets on a global reset or power cycle, during which the
// DbC link drops; reconnect and use attach (or wait) afterwards.
func (c *Core) SetResetBreak(arm bool) error {
	v := int64(0)
	if arm {
		v = 1
	}
	_, err := c.s.IRDR(c.tap, irResetBrk, 1, big.NewInt(v))
	return err
}

// Wait polls until the core enters probe mode by itself (breakpoint, step,
// or someone else's halt), up to timeout. It reports whether it stopped.
func (c *Core) Wait(timeout time.Duration) (runctl.StopInfo, bool, error) {
	if c.halted {
		si, err := c.stopInfo("already halted")
		return si, true, err
	}
	deadline := time.Now().Add(timeout)
	for {
		st, err := c.status()
		if err != nil {
			return runctl.StopInfo{}, false, err
		}
		if st&stInPM != 0 && st&stPMReq == 0 {
			si, err := c.entered()
			return si, err == nil, err
		}
		if time.Now().After(deadline) {
			return runctl.StopInfo{}, false, nil
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// BreakKind is what a hardware breakpoint triggers on.
type BreakKind int

const (
	BreakExec BreakKind = iota
	BreakWrite
	BreakIO
	BreakAccess // read or write
)

type breakpoint struct {
	addr   uint32
	kind   BreakKind
	length int
}

func (bp *breakpoint) dr7Bits(i int) uint32 {
	lenCode := map[int]uint32{1: 0, 2: 1, 8: 2, 4: 3}[bp.length]
	if bp.kind == BreakExec {
		lenCode = 0
	}
	return 1<<(2*i) | (uint32(bp.kind)|lenCode<<2)<<(16+4*i) // local enable + RW/LEN
}

// SetBreakpoint arms hardware breakpoint slot 0..3 at a linear address. It
// takes effect on the next resume. kind: "x", "w", "rw" or "io".
func (c *Core) SetBreakpoint(slot int, addr uint64, kindName string, length int) error {
	kind, ok := map[string]BreakKind{"x": BreakExec, "w": BreakWrite, "rw": BreakAccess, "io": BreakIO}[kindName]
	if !ok {
		return fmt.Errorf("lakemont: breakpoint kind must be x, w, rw or io")
	}
	if kind == BreakExec {
		length = 1
	}
	if slot < 0 || slot > 3 {
		return fmt.Errorf("lakemont: breakpoint slot must be 0..3")
	}
	if kind != BreakExec && length != 1 && length != 2 && length != 4 && length != 8 {
		return fmt.Errorf("lakemont: data breakpoint length must be 1, 2, 4 or 8")
	}
	if kind != BreakExec && addr%uint64(length) != 0 {
		return fmt.Errorf("lakemont: data breakpoint address must be aligned to its length")
	}
	c.bps[slot] = &breakpoint{addr: uint32(addr), kind: kind, length: length}
	return nil
}

// Armed reports whether any breakpoint is set or still programmed.
func (c *Core) Armed() bool {
	for _, bp := range c.bps {
		if bp != nil {
			return true
		}
	}
	return c.progDR7 != nil
}

// ClearBreakpoint disarms a slot (on the next resume).
func (c *Core) ClearBreakpoint(slot int) error {
	if slot < 0 || slot > 3 {
		return fmt.Errorf("lakemont: breakpoint slot must be 0..3")
	}
	c.bps[slot] = nil
	return nil
}

// ForceResume leaves probe mode after restoring only what this session
// saved; for cores adopted with Attach whose state the user fixed up.
func (c *Core) ForceResume() error {
	if err := c.needHalted(); err != nil {
		return err
	}
	if err := c.restore(); err != nil {
		return err
	}
	return c.exitProbeMode(true)
}

// restore writes back saved program values.
func (c *Core) restore() error {
	names := make([]string, 0, len(c.orig))
	for r := range c.orig {
		names = append(names, r)
	}
	slices.Sort(names)
	// Order doesn't matter: shadow RAM only takes effect on probe-mode exit.
	_, err := c.run(func(b *batch) {
		for _, r := range names {
			b.writeReg(r, c.orig[r])
		}
		b.checkStatus()
	})
	return err
}

// exitProbeMode releases the core. With waitExit it confirms the core left
// probe mode; a single step re-enters it immediately, so that's skipped.
func (c *Core) exitProbeMode(waitExit bool) error {
	if _, err := c.s.IRDR(c.tap, irProbeMode, 1, big.NewInt(0)); err != nil {
		return err
	}
	c.halted, c.prepared, c.orig = false, false, nil
	if c.progDR7 == nil {
		c.owned = false
		c.forget()
	} else {
		// Breakpoints are armed: remember the program's DR7/PMCR so a later
		// session can disarm them cleanly.
		c.persist()
	}
	if !waitExit {
		return nil
	}
	for i := 0; i < 100; i++ {
		st, err := c.status()
		if err != nil {
			return err
		}
		if st&stInPM == 0 {
			return nil
		}
	}
	return fmt.Errorf("lakemont: %s did not leave probe mode", c.name)
}

// The program values the debugger replaces are also kept in a state file,
// so that a later session can still resume the core correctly if this one
// dies while the core is halted.
func (c *Core) stateFile() string {
	dir, err := os.UserCacheDir()
	if err != nil {
		dir = os.TempDir()
	}
	return filepath.Join(dir, "libredci", "lakemont-"+c.name+".json")
}

type savedState struct {
	Orig     map[string]uint32 `json:"orig"`
	ProgDR7  *uint32           `json:"prog_dr7,omitempty"`
	ProgPMCR *uint32           `json:"prog_pmcr,omitempty"`
	Stepping bool              `json:"stepping,omitempty"`
	ProgDR   map[int]uint32    `json:"prog_dr,omitempty"`
}

func (c *Core) persist() error {
	p := c.stateFile()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(savedState{c.orig, c.progDR7, c.progPMCR, c.stepping, c.progDR}, "", "  ")
	return os.WriteFile(p, b, 0o644)
}

func (c *Core) forget() { os.Remove(c.stateFile()) }

// Attach adopts a core that is already in probe mode (e.g. left halted by
// an earlier session) without changing anything. If a state file from the
// session that halted it exists, its saved program values are loaded and
// Resume works normally; otherwise the pre-halt state is unknown and only
// ForceResume will let it run.
func (c *Core) Attach() error {
	h, err := c.Halted()
	if err != nil {
		return err
	}
	if !h {
		return fmt.Errorf("lakemont: %s is running; use halt", c.name)
	}
	st, err := c.loadState()
	switch {
	case err == nil && st.Orig != nil:
		// Halted by an earlier session that saved its state.
		c.halted, c.owned = true, true
		c.orig, c.progDR7, c.progPMCR, c.progDR = st.Orig, st.ProgDR7, st.ProgPMCR, st.ProgDR
	case err == nil && st.ProgDR7 != nil:
		// Stopped by itself (breakpoint/step) after an earlier session
		// resumed it: this is a normal stop, take it over properly.
		c.progDR7, c.progPMCR, c.stepping, c.progDR = st.ProgDR7, st.ProgPMCR, st.Stepping, st.ProgDR
		_, err := c.entered()
		return err
	default:
		st, err := c.status()
		if err != nil {
			return err
		}
		if st&stResetBrk != 0 {
			// Stopped at the reset vector: the current state is the
			// program state, so this is a normal, resumable halt.
			c.forget()
			_, err := c.entered()
			return err
		}
		c.halted, c.owned, c.orig = true, false, map[string]uint32{}
	}
	return nil
}

func (c *Core) loadState() (savedState, error) {
	var st savedState
	b, err := os.ReadFile(c.stateFile())
	if err != nil {
		return st, err
	}
	if err := json.Unmarshal(b, &st); err != nil {
		return st, fmt.Errorf("lakemont: %s: bad state file %s: %w", c.name, c.stateFile(), err)
	}
	return st, nil
}

// Owned reports whether Resume can restore the pre-halt state.
func (c *Core) Owned() bool { return c.owned }

// ---- registers ----

func (c *Core) ReadRegister(name string) (uint64, error) {
	v, err := c.ReadRegisters([]string{name})
	if err != nil {
		return 0, err
	}
	return v[0], nil
}

func (c *Core) ReadRegisters(names []string) ([]uint64, error) {
	if err := c.needHalted(); err != nil {
		return nil, err
	}
	for _, n := range names {
		if _, ok := regSelect[n]; !ok {
			return nil, fmt.Errorf("lakemont: unknown register %q", n)
		}
	}
	out := make([]uint64, len(names))
	var hw []string
	for _, n := range names {
		if _, saved := c.orig[n]; !saved {
			hw = append(hw, n)
		}
	}
	var vals []uint32
	for i := 0; i < len(hw); i += regChunk {
		part := hw[i:min(i+regChunk, len(hw))]
		v, err := c.run(func(b *batch) {
			for _, n := range part {
				b.readReg(n)
			}
		})
		if err != nil {
			return nil, err
		}
		vals = append(vals, v...)
	}
	for i, n := range names {
		if v, saved := c.orig[n]; saved {
			out[i] = uint64(v)
		} else {
			out[i] = uint64(vals[slices.Index(hw, n)])
		}
		if slot, ok := drSlot(n); ok {
			if v, used := c.progDR[slot]; used {
				out[i] = uint64(v) // the live register is the debugger's
			}
		}
	}
	return out, nil
}

// WriteRegister changes the program-visible value. For registers the
// debugger itself is using (segment setup, cr0, eax/edx after memory
// accesses) the new value takes effect on resume.
func (c *Core) WriteRegister(name string, v uint64) error {
	if err := c.needHalted(); err != nil {
		return err
	}
	if _, ok := regSelect[name]; !ok {
		return fmt.Errorf("lakemont: unknown register %q", name)
	}
	if slot, ok := drSlot(name); ok {
		if _, used := c.progDR[slot]; used {
			c.progDR[slot] = uint32(v) // written once the slot is free
			return c.persist()
		}
	}
	if _, saved := c.orig[name]; saved {
		c.orig[name] = uint32(v)
		return c.persist()
	}
	_, err := c.run(func(b *batch) { b.writeReg(name, uint32(v)); b.checkStatus() })
	return err
}

// drSlot returns n for "drN" (N = 0..3).
func drSlot(name string) (int, bool) {
	if len(name) == 3 && name[:2] == "dr" && name[2] >= '0' && name[2] <= '3' {
		return int(name[2] - '0'), true
	}
	return 0, false
}

// claim saves a register's program value before the debugger uses it.
func (c *Core) claim(names ...string) error {
	var need []string
	for _, n := range names {
		if _, ok := c.orig[n]; !ok {
			need = append(need, n)
		}
	}
	if len(need) == 0 {
		return nil
	}
	v, err := c.run(func(b *batch) {
		for _, n := range need {
			b.readReg(n)
		}
	})
	if err != nil {
		return err
	}
	for i, n := range need {
		c.orig[n] = v[i]
	}
	return c.persist()
}

// ---- memory and I/O ----

const (
	chunk    = 16 // memory operations per JTAG transaction
	regChunk = 8  // register reads per JTAG transaction
)

func memRd(size int) uint64 {
	return map[int]uint64{1: pirMemRd8, 2: pirMemRd16, 4: pirMemRd32}[size]
}

func memWr(size int) uint64 {
	return map[int]uint64{1: pirMemWr8, 2: pirMemWr16, 4: pirMemWr32}[size]
}

// split breaks [addr, addr+n) into naturally aligned accesses.
func split(addr uint64, n int) (out [][2]int) {
	off := 0
	for off < n {
		a := addr + uint64(off)
		size := 1
		if a%4 == 0 && n-off >= 4 {
			size = 4
		} else if a%2 == 0 && n-off >= 2 {
			size = 2
		}
		out = append(out, [2]int{off, size})
		off += size
	}
	return out
}

func (c *Core) ReadMemory(addr uint64, buf []byte) error {
	if err := c.needHalted(); err != nil {
		return err
	}
	if addr+uint64(len(buf)) > 1<<32 {
		return fmt.Errorf("lakemont: address beyond 4 GiB")
	}
	if err := c.claim("eax", "edx"); err != nil {
		return err
	}
	acc := split(addr, len(buf))
	for i := 0; i < len(acc); i += chunk {
		part := acc[i:min(i+chunk, len(acc))]
		vals, err := c.run(func(b *batch) {
			for _, a := range part {
				b.writeReg("eax", uint32(addr)+uint32(a[0]))
				b.submitBus(memRd(a[1]))
				b.readReg("edx")
			}
			b.checkStatus()
		})
		if err != nil {
			return fmt.Errorf("lakemont: read at %#x: %w", addr+uint64(part[0][0]), err)
		}
		for j, a := range part {
			var tmp [4]byte
			binary.LittleEndian.PutUint32(tmp[:], vals[j])
			copy(buf[a[0]:a[0]+a[1]], tmp[:a[1]])
		}
	}
	return nil
}

func (c *Core) WriteMemory(addr uint64, buf []byte) error {
	if err := c.needHalted(); err != nil {
		return err
	}
	if addr+uint64(len(buf)) > 1<<32 {
		return fmt.Errorf("lakemont: address beyond 4 GiB")
	}
	if err := c.claim("eax", "edx"); err != nil {
		return err
	}
	acc := split(addr, len(buf))
	for i := 0; i < len(acc); i += chunk {
		part := acc[i:min(i+chunk, len(acc))]
		_, err := c.run(func(b *batch) {
			for _, a := range part {
				var tmp [4]byte
				copy(tmp[:], buf[a[0]:a[0]+a[1]])
				b.writeReg("eax", uint32(addr)+uint32(a[0]))
				b.writeReg("edx", binary.LittleEndian.Uint32(tmp[:]))
				b.submitBus(memWr(a[1]))
			}
			b.checkStatus()
		})
		if err != nil {
			return fmt.Errorf("lakemont: write at %#x: %w", addr+uint64(part[0][0]), err)
		}
	}
	return nil
}

func ioOp(size int, write bool) (uint64, error) {
	ops := map[int][2]uint64{1: {pirIORd8, pirIOWr8}, 2: {pirIORd16, pirIOWr16}, 4: {pirIORd32, pirIOWr32}}
	o, ok := ops[size]
	if !ok {
		return 0, fmt.Errorf("lakemont: bad I/O size %d", size)
	}
	if write {
		return o[1], nil
	}
	return o[0], nil
}

func (c *Core) IORead(port uint16, size int) (uint32, error) {
	if err := c.needHalted(); err != nil {
		return 0, err
	}
	op, err := ioOp(size, false)
	if err != nil {
		return 0, err
	}
	if err := c.claim("eax", "edx"); err != nil {
		return 0, err
	}
	v, err := c.run(func(b *batch) {
		b.writeReg("edx", uint32(port))
		b.submitBus(op)
		b.readReg("eax")
		b.checkStatus()
	})
	if err != nil {
		return 0, err
	}
	return v[0] & uint32(uint64(1)<<(8*size)-1), nil
}

func (c *Core) IOWrite(port uint16, size int, v uint32) error {
	if err := c.needHalted(); err != nil {
		return err
	}
	op, err := ioOp(size, true)
	if err != nil {
		return err
	}
	if err := c.claim("eax", "edx"); err != nil {
		return err
	}
	_, err = c.run(func(b *batch) {
		b.writeReg("edx", uint32(port))
		b.writeReg("eax", v)
		b.submitBus(op)
		b.checkStatus()
	})
	return err
}

// ---- MSRs ----

// KnownMSRs are MSRs verified to exist on the CSME's Lakemont core. An
// RDMSR/WRMSR of an unimplemented MSR faults inside probe mode, which on
// the T480 powered the whole platform off; others need force.
var KnownMSRs = map[uint32]string{
	0x10: "TSC",
}

// msrOK refuses MSR access when the code segment selector's RPL isn't 0,
// which crashes some Lakemont versions.
func (c *Core) msrOK() error {
	cs, err := c.ReadRegister("cs")
	if err != nil {
		return err
	}
	if cs&3 != 0 {
		return fmt.Errorf("lakemont: %s: MSR access with CS.RPL=%d is unsafe on Lakemont", c.name, cs&3)
	}
	return c.claim("eax", "ecx", "edx")
}

// ReadMSR executes RDMSR with ECX = index (only for KnownMSRs).
func (c *Core) ReadMSR(index uint32) (uint64, error) { return c.readMSR(index, false) }

// ReadMSRForce reads an MSR not known to exist. If it doesn't, the fault
// in probe mode can take the platform down.
func (c *Core) ReadMSRForce(index uint32) (uint64, error) { return c.readMSR(index, true) }

func (c *Core) readMSR(index uint32, force bool) (uint64, error) {
	if err := c.needHalted(); err != nil {
		return 0, err
	}
	if _, ok := KnownMSRs[index]; !ok && !force {
		return 0, fmt.Errorf("lakemont: MSR %#x is not known to exist on this core; an access fault in probe mode can power off the platform (force to override)", index)
	}
	if err := c.msrOK(); err != nil {
		return 0, err
	}
	v, err := c.run(func(b *batch) {
		b.writeReg("ecx", index)
		b.submitBus(pirRDMSR)
		b.readReg("eax")
		b.readReg("edx")
		b.checkStatus()
	})
	if err != nil {
		return 0, fmt.Errorf("lakemont: rdmsr %#x: %w", index, err)
	}
	return uint64(v[1])<<32 | uint64(v[0]), nil
}

// WriteMSR executes WRMSR with ECX = index, EDX:EAX = v (KnownMSRs only).
func (c *Core) WriteMSR(index uint32, v uint64) error { return c.writeMSR(index, v, false) }

// WriteMSRForce writes an MSR not known to exist (see ReadMSRForce).
func (c *Core) WriteMSRForce(index uint32, v uint64) error { return c.writeMSR(index, v, true) }

func (c *Core) writeMSR(index uint32, v uint64, force bool) error {
	if err := c.needHalted(); err != nil {
		return err
	}
	if _, ok := KnownMSRs[index]; !ok && !force {
		return fmt.Errorf("lakemont: MSR %#x is not known to exist on this core; an access fault in probe mode can power off the platform (force to override)", index)
	}
	if err := c.msrOK(); err != nil {
		return err
	}
	_, err := c.run(func(b *batch) {
		b.writeReg("ecx", index)
		b.writeReg("eax", uint32(v))
		b.writeReg("edx", uint32(v>>32))
		b.submitBus(pirWRMSR)
		b.checkStatus()
	})
	if err != nil {
		return fmt.Errorf("lakemont: wrmsr %#x: %w", index, err)
	}
	return nil
}

// ---- linear addresses ----

// Translate maps a linear address to physical using the program's paging
// setup (32-bit paging with optional 4 MiB pages; PAE is not supported).
func (c *Core) Translate(lin uint64) (uint64, error) {
	r, err := c.ReadRegisters([]string{"cr0", "cr3", "cr4"})
	if err != nil {
		return 0, err
	}
	cr0, cr3, cr4 := uint32(r[0]), uint32(r[1]), uint32(r[2])
	if cr0&cr0PG == 0 {
		return lin, nil
	}
	if cr4&(1<<5) != 0 {
		return 0, fmt.Errorf("lakemont: PAE paging not supported")
	}
	var b [4]byte
	pdeAddr := uint64(cr3&^0xfff) + (lin>>22)*4
	if err := c.ReadMemory(pdeAddr, b[:]); err != nil {
		return 0, err
	}
	pde := binary.LittleEndian.Uint32(b[:])
	if pde&1 == 0 {
		return 0, fmt.Errorf("lakemont: %#x not mapped (PDE %#x)", lin, pde)
	}
	if pde&(1<<7) != 0 && cr4&(1<<4) != 0 { // 4 MiB page
		return uint64(pde&0xffc00000) | lin&0x3fffff, nil
	}
	pteAddr := uint64(pde&^0xfff) + (lin>>12&0x3ff)*4
	if err := c.ReadMemory(pteAddr, b[:]); err != nil {
		return 0, err
	}
	pte := binary.LittleEndian.Uint32(b[:])
	if pte&1 == 0 {
		return 0, fmt.Errorf("lakemont: %#x not mapped (PTE %#x)", lin, pte)
	}
	return uint64(pte&^0xfff) | lin&0xfff, nil
}

// ReadLinear reads memory at a linear address (page by page).
func (c *Core) ReadLinear(lin uint64, buf []byte) error {
	return c.linear(lin, buf, c.ReadMemory)
}

// WriteLinear writes memory at a linear address.
func (c *Core) WriteLinear(lin uint64, buf []byte) error {
	return c.linear(lin, buf, c.WriteMemory)
}

func (c *Core) linear(lin uint64, buf []byte, op func(uint64, []byte) error) error {
	for off := 0; off < len(buf); {
		a := lin + uint64(off)
		n := min(len(buf)-off, int(0x1000-a&0xfff))
		pa, err := c.Translate(a)
		if err != nil {
			return err
		}
		if err := op(pa, buf[off:off+n]); err != nil {
			return err
		}
		off += n
	}
	return nil
}
