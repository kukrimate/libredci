// SPDX-License-Identifier: GPL-2.0-only
package bigcore

import (
	"encoding/binary"
	"fmt"
)

// Memory and I/O are accessed from a flat 64-bit environment: the halted
// thread is switched to a 64-bit code segment with paging turned off
// (through a probe-mode-only control), so addresses are physical. Every
// change is undone before the thread resumes.

const (
	msrProbeStage = 0x173 // staging register for segment bases and similar
	msrProbePG    = 0x77  // probe-mode paging control: 0 = off, 0x80000000 = on
	msrEPTP       = 0x510
	msrEFER       = 0xc0000080

	segES, segCS, segSS, segIDT, segGDT, segTR, segLDT = 1, 3, 5, 0xc, 0xd, 0xe, 0xf
)

type flatState struct {
	scp11, scp12, scp13 uint64
	eptp                uint64
	eptpOK              bool
	efer, cr4, cr3      uint64
	rsi, rdi            uint64
	seg                 map[int][2]uint64 // base, attributes/limit word (from SCP1)
}

// segment selects/restores order used on restore.
var flatSegs = []int{segGDT, segIDT, segSS, segCS, segTR, segLDT}

// csUpdate rewrites the CS attributes (bits 31:16 of SCP1 after reading CS).
func csUpdate(p *prog, e enc, ar uint32) {
	p.exec(e.movRR(rAX, rSP), e.movR32Imm(rCX, msrProbeSeg), e.movR32Imm(rAX, segCS), e.rdmsr())
	p.exec(e.movR32Imm(rCX, msrSCP0+1), e.rdmsr())
	p.exec(e.andEAXImm(0xffff), e.movR32Imm(rCX, ar<<16), e.orEAXECX(), e.movR32Imm(rCX, msrSCP0+1), e.wrmsr())
	p.exec(e.movR32Imm(rCX, msrProbeSeg), e.movR32Imm(rAX, segCS), e.wrmsr())
}

func (t *Thread) entryMode() int {
	csar, efer := t.entry>>48&0xffff, t.entry&0xffff
	switch {
	case efer&(1<<10) != 0 && csar&(1<<13) != 0:
		return 64
	case csar&(1<<14) != 0:
		return 32
	}
	return 16
}

func (t *Thread) enterFlat64() error {
	if t.flat != nil {
		return nil
	}
	if t.rflags&(1<<17) != 0 {
		return fmt.Errorf("bigcore: %s: memory access from virtual-8086 mode is not supported", t.name)
	}
	f := &flatState{seg: map[int][2]uint64{}}
	var err error
	if f.efer, err = t.readMSR(msrEFER); err != nil {
		return err
	}
	if f.eptp, err = t.readMSR(msrEPTP); err == nil {
		f.eptpOK = true
	}

	// Save what the switch changes.
	vals, err := t.run(func(p *prog, e enc) {
		for i := 11; i <= 13; i++ {
			p.exec(e.movR8Imm(rCL, msrProbeToPDR), e.movR8Imm(rCH, 0), e.movR8Imm(rAL, byte(i)), e.movR8Imm(rAH, 0), e.rdmsr())
			p.pdr()
		}
		p.exec(e.movFromCR(rAX, 4), e.movR32Imm(rCX, msrProbeGPReg), e.rdmsr())
		p.pdr()
		for _, s := range []int{segCS, segGDT, segIDT, segTR, segLDT} {
			p.exec(e.movR32Imm(rCX, msrProbeSeg), e.movR32Imm(rAX, uint32(s)), e.rdmsr())
			p.pdr()
			p.exec(e.movR32Imm(rAX, msrSCP0+1), e.movR32Imm(rCX, msrProbeToPDR), e.rdmsr())
			p.pdr()
		}
		// SS: also keep the staging register (in SCP12, saved above).
		p.exec(e.movR32Imm(rCX, msrProbeSeg), e.movR32Imm(rAX, segSS), e.rdmsr())
		p.pdr()
		p.exec(e.movR32Imm(rCX, msrProbeStage), e.rdmsr())
		p.exec(e.movR32Imm(rCX, msrSCP0+12), e.wrmsr())
		p.exec(e.movR32Imm(rAX, msrSCP0+1), e.movR32Imm(rCX, msrProbeToPDR), e.rdmsr())
		p.pdr()
	})
	if err != nil {
		return fmt.Errorf("bigcore: %s: saving state for memory access: %w", t.name, err)
	}
	f.scp11, f.scp12, f.scp13 = vals[0], vals[1], vals[2]
	f.cr4 = vals[3]
	for i, s := range []int{segCS, segGDT, segIDT, segTR, segLDT} {
		f.seg[s] = [2]uint64{vals[4+2*i], vals[5+2*i]}
	}
	f.seg[segSS] = [2]uint64{vals[14], vals[15]}
	t.flat = f // from here on, leaveFlat64 must run before resuming

	lma := t.entry&(1<<10) != 0
	csar := uint32(t.entry >> 48 & 0xffff)
	cr0 := uint32(t.scp[9])
	_, err = t.run(func(p *prog, e enc) {
		// SS: flat writable data segment.
		p.exec(e.movRR(rAX, rSP), e.movR32Imm(rCX, msrSCP0+12), e.rdmsr())
		p.exec(e.movR32Imm(rCX, msrProbeStage), e.wrmsr())
		p.exec(e.movR32Imm(rCX, msrSCP0+1), e.rdmsr())
		p.exec(e.andEAXImm(0xffff), e.movR32Imm(rCX, 0x40930000), e.orEAXECX(), e.movR32Imm(rCX, msrSCP0+1), e.wrmsr())
		p.exec(e.movR32Imm(rCX, msrProbeSeg), e.movR32Imm(rAX, segSS), e.wrmsr())
		if !lma {
			// Protected mode without paging, 16-bit CS, empty TSS, then
			// enable long mode.
			p.exec(e.movR32Imm(rAX, cr0&0x7fffffff|1), e.movToCR(0, rAX))
			csUpdate(p, e, csar&0x9fff)
			e = enc{16}
			p.exec(e.movR32Imm(rAX, 0), e.movR32Imm(rDX, 0), e.movR32Imm(rCX, msrProbeStage), e.wrmsr())
			p.exec(e.movR32Imm(rAX, 0x48890000), e.movR32Imm(rDX, 0xffff), e.movR32Imm(rCX, msrSCP0+1), e.wrmsr())
			p.exec(e.movR32Imm(rCX, msrProbeSeg), e.movR32Imm(rAX, segTR), e.wrmsr())
			p.exec(e.movFromCR(rAX, 4), e.orEAXImm(0x20), e.movToCR(4, rAX))
			p.exec(e.movR32Imm(rCX, msrEFER), e.rdmsr(), e.orEAXImm(0x100), e.wrmsr())
			p.exec(e.movR32Imm(rAX, cr0|0x80000001), e.movToCR(0, rAX))
		}
		// 64-bit code segment.
		csUpdate(p, e, (csar|0x2000)&0xbfff)
		e = enc{64}
		// Flush TLBs, clear CR0.TS, paging off, EPT off.
		p.exec(e.movFromCR(rAX, 3), e.movToCR(3, rAX), e.movFromCR(rAX, 4), e.xorEAXImm(0x80), e.movToCR(4, rAX), e.xorEAXImm(0x80), e.movToCR(4, rAX))
		p.exec(e.movFromCR(rAX, 0), e.andRAXImm(^8), e.movToCR(0, rAX))
		p.exec(e.movR32Imm(rCX, msrProbePG), e.movR32Imm(rDX, 0), e.movR32Imm(rAX, 0), e.wrmsr())
		if f.eptpOK {
			p.exec(e.movR32Imm(rCX, msrEPTP), e.rdmsr(), e.andEDXImm(0x7fffffff), e.wrmsr())
		}
	})
	t.mode = 64
	if err != nil {
		return fmt.Errorf("bigcore: %s: switching to 64-bit mode: %w", t.name, err)
	}
	// RSI, RDI and CR3 in full, now that 64-bit encodings apply.
	vals, err = t.run(func(p *prog, e enc) {
		for _, r := range []int{rSI, rDI} {
			p.exec(e.movRR(rAX, r), e.movR32Imm(rCX, msrProbeGPReg), e.rdmsr())
			p.pdr()
		}
		p.exec(e.movFromCR(rAX, 3), e.movR32Imm(rCX, msrProbeGPReg), e.rdmsr())
		p.pdr()
	})
	if err != nil {
		return fmt.Errorf("bigcore: %s: saving state for memory access: %w", t.name, err)
	}
	f.rsi, f.rdi, f.cr3 = vals[0], vals[1], vals[2]
	return nil
}

func (t *Thread) leaveFlat64() error {
	f := t.flat
	if f == nil {
		return nil
	}
	lma := t.entry&(1<<10) != 0
	csar := uint32(t.entry >> 48 & 0xffff)
	cr0 := t.scp[9]
	pg := cr0&(1<<31) != 0
	final := t.entryMode()
	_, err := t.run(func(p *prog, e enc) {
		e = enc{64}
		p.exec(e.movR32Imm(rCX, msrICECTLPMR), e.rdmsr(), e.andEAXImm(^uint32(iceFault)), e.wrmsr())
		p.exec(e.movR64Imm(rDI, f.rdi))
		p.exec(e.movR64Imm(rSI, f.rsi))
		p.exec(e.movR64Imm(rAX, f.cr3), e.movToCR(3, rAX))
		if !lma || pg {
			p.exec(e.movR32Imm(rCX, msrProbePG), e.movR32Imm(rAX, 0x80000000), e.movR32Imm(rDX, 0), e.wrmsr())
		}
		if !lma {
			csUpdate(p, e, csar&0x9fff)
			e = enc{16}
			p.exec(e.movFromCR(rAX, 0), e.andEAXImm(0x7fffffff), e.movToCR(0, rAX))
		}
		p.exec(e.movR32Imm(rCX, msrEFER), e.movR32Imm(rAX, uint32(f.efer)), e.movR32Imm(rDX, uint32(f.efer>>32)), e.wrmsr())
		p.exec(e.movImm(rAX, f.cr4), e.movToCR(4, rAX))
		p.exec(e.movImm(rAX, cr0), e.movToCR(0, rAX))
		for _, s := range flatSegs {
			v := f.seg[s]
			p.exec(e.movR32Imm(rCX, msrProbeSeg), e.movR32Imm(rAX, uint32(s)), e.rdmsr())
			p.exec(e.movR32Imm(rAX, uint32(v[0])), e.movR32Imm(rDX, uint32(v[0]>>32)), e.movR32Imm(rCX, msrProbeStage), e.wrmsr())
			p.exec(e.movR32Imm(rAX, uint32(v[1])), e.movR32Imm(rDX, uint32(v[1]>>32)), e.movR32Imm(rCX, msrSCP0+1), e.wrmsr())
			p.exec(e.movR32Imm(rCX, msrProbeSeg), e.movR32Imm(rAX, uint32(s)), e.wrmsr())
			if s == segCS {
				e = enc{final} // CS is back: original mode
			}
		}
		if f.eptpOK {
			p.exec(e.movR32Imm(rCX, msrEPTP), e.movR32Imm(rAX, uint32(f.eptp)), e.movR32Imm(rDX, uint32(f.eptp>>32)), e.wrmsr())
		}
		for i, v := range []uint64{f.scp11, f.scp12, f.scp13} {
			p.exec(e.movR32Imm(rCX, uint32(msrSCP0+11+i)), e.movR32Imm(rAX, uint32(v)), e.movR32Imm(rDX, uint32(v>>32)), e.wrmsr())
		}
	})
	if err != nil {
		return fmt.Errorf("bigcore: %s: leaving 64-bit mode: %w", t.name, err)
	}
	t.mode = final
	t.flat = nil
	return nil
}

// faultCheck appends a check of the probe-mode fault flag; it must be the
// last PDR read of the program.
func faultCheck(p *prog, e enc) {
	p.exec(e.movR32Imm(rCX, msrICECTLPMR), e.rdmsr(), e.andEAXImm(iceFault), e.shrImm(rAX, 4), e.movR32Imm(rCX, msrProbeGPReg), e.rdmsr())
	p.pdr()
}

func clearFault(p *prog, e enc) {
	p.exec(e.movR32Imm(rCX, msrICECTLPMR), e.rdmsr(), e.andEAXImm(^uint32(iceFault)), e.wrmsr())
}

// split breaks [addr, addr+n) into naturally aligned accesses of up to 4
// bytes. Some MMIO registers hang the core on an 8-byte load (on KBL-R, the
// one at MCHBAR+0x8).
func split(addr uint64, n int) (out [][2]int) {
	for off := 0; off < n; {
		a := addr + uint64(off)
		size := 1
		for _, s := range []int{4, 2} {
			if a%uint64(s) == 0 && n-off >= s {
				size = s
				break
			}
		}
		out = append(out, [2]int{off, size})
		off += size
	}
	return out
}

const memChunk = 32 // accesses per JTAG transaction

// checkAddr rejects addresses in the non-canonical range unless CR4.LA57
// is set, as OpenIPC does for physical accesses.
func (t *Thread) checkAddr(addr uint64, n int) error {
	if t.flat.cr4&(1<<12) != 0 {
		return nil
	}
	for _, a := range []uint64{addr, addr + uint64(n) - 1} {
		if a > 0x7fffffffffff && a < 0xffff800000000000 {
			return fmt.Errorf("bigcore: address %#x is beyond 48 bits and the thread is not using 5-level paging", a)
		}
	}
	return nil
}

func (t *Thread) ReadMemory(addr uint64, buf []byte) error {
	if err := t.need(); err != nil {
		return err
	}
	if err := t.enterFlat64(); err != nil {
		return err
	}
	if err := t.checkAddr(addr, len(buf)); err != nil {
		return err
	}
	acc := split(addr, len(buf))
	for i := 0; i < len(acc); i += memChunk {
		part := acc[i:min(i+memChunk, len(acc))]
		v, err := t.run(func(p *prog, e enc) {
			clearFault(p, e)
			for _, a := range part {
				p.exec(e.movR64Imm(rDX, addr+uint64(a[0])), e.loadRDX(a[1]), e.movR32Imm(rCX, msrProbeGPReg), e.rdmsr())
				p.pdr()
			}
			faultCheck(p, e)
		})
		if err != nil {
			return err
		}
		if v[len(v)-1]&1 != 0 {
			return fmt.Errorf("bigcore: memory read at %#x faulted", addr+uint64(part[0][0]))
		}
		for j, a := range part {
			var tmp [8]byte
			binary.LittleEndian.PutUint64(tmp[:], v[j])
			copy(buf[a[0]:a[0]+a[1]], tmp[:a[1]])
		}
	}
	return nil
}

func (t *Thread) WriteMemory(addr uint64, buf []byte) error {
	if err := t.need(); err != nil {
		return err
	}
	if err := t.enterFlat64(); err != nil {
		return err
	}
	if err := t.checkAddr(addr, len(buf)); err != nil {
		return err
	}
	acc := split(addr, len(buf))
	for i := 0; i < len(acc); i += memChunk {
		part := acc[i:min(i+memChunk, len(acc))]
		v, err := t.run(func(p *prog, e enc) {
			clearFault(p, e)
			for _, a := range part {
				var tmp [8]byte
				copy(tmp[:], buf[a[0]:a[0]+a[1]])
				p.exec(e.movR64Imm(rAX, binary.LittleEndian.Uint64(tmp[:])), e.movR64Imm(rDX, addr+uint64(a[0])), e.storeRDX(a[1]))
			}
			faultCheck(p, e)
		})
		if err != nil {
			return err
		}
		if v[len(v)-1]&1 != 0 {
			return fmt.Errorf("bigcore: memory write at %#x faulted", addr+uint64(part[0][0]))
		}
	}
	return nil
}

func ioSizeOK(size int) error {
	if size != 1 && size != 2 && size != 4 {
		return fmt.Errorf("bigcore: bad I/O size %d", size)
	}
	return nil
}

func (t *Thread) IORead(port uint16, size int) (uint32, error) {
	if err := ioSizeOK(size); err != nil {
		return 0, err
	}
	if err := t.need(); err != nil {
		return 0, err
	}
	if err := t.enterFlat64(); err != nil {
		return 0, err
	}
	v, err := t.run(func(p *prog, e enc) {
		clearFault(p, e)
		p.exec(e.movR32Imm(rDX, uint32(port)), e.movR32Imm(rAX, 0), e.in(size), e.movR32Imm(rCX, msrProbeGPReg), e.rdmsr())
		p.pdr()
		faultCheck(p, e)
	})
	if err != nil {
		return 0, err
	}
	if v[1]&1 != 0 {
		return 0, fmt.Errorf("bigcore: I/O read of port %#x faulted", port)
	}
	return uint32(v[0]) & uint32(uint64(1)<<(8*size)-1), nil
}

func (t *Thread) IOWrite(port uint16, size int, val uint32) error {
	if err := ioSizeOK(size); err != nil {
		return err
	}
	if err := t.need(); err != nil {
		return err
	}
	if err := t.enterFlat64(); err != nil {
		return err
	}
	v, err := t.run(func(p *prog, e enc) {
		clearFault(p, e)
		p.exec(e.movR32Imm(rAX, val), e.movR32Imm(rDX, uint32(port)), e.out(size))
		faultCheck(p, e)
	})
	if err != nil {
		return err
	}
	if v[0]&1 != 0 {
		return fmt.Errorf("bigcore: I/O write to port %#x faulted", port)
	}
	return nil
}
