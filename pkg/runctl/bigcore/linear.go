// SPDX-License-Identifier: GPL-2.0-only
package bigcore

import (
	"encoding/binary"
	"fmt"
)

// CodeMode is the operand mode (16, 32 or 64) the stopped thread executes in.
func (t *Thread) CodeMode() int { return t.entryMode() }

const (
	msrVMXEPTVPIDCap = 0x48c // IA32_VMX_EPT_VPID_CAP
	scp6VMXGuest     = 1 << 62
	entryPostVMXON   = 1 << 27
	eptpEnable       = 1 << 63
)

// paging is what a linear-to-physical translation depends on.
type paging struct {
	cr0, cr3, cr4, efer uint64
	maxPhys             uint // physical address width (CPUID 0x80000008 EAX[7:0])
	pat, pse36, gbPages bool // CPUID 1 EDX[16], 1 EDX[17], 0x80000001 EDX[26]
	ept                 bool // VMX guest with EPT: table and final addresses are guest-physical
	eptp, eptCap        uint64
}

// cpuid runs CPUID with EAX = leaf and ECX = sub on the thread and returns
// EAX, EBX, ECX and EDX. As in OpenIPC, CPUID runs once per register read
// (moving a register to the PDR overwrites ECX).
func (t *Thread) cpuid(leaf, sub uint32) ([4]uint32, error) {
	v, err := t.run(func(p *prog, e enc) {
		for _, r := range []int{rAX, rBX, rCX, rDX} {
			p.exec(e.movR32Imm(rCX, sub), e.movR32Imm(rAX, leaf), []byte{0x0f, 0xa2})
			if r != rAX {
				p.exec(e.movRR(rAX, r))
			}
			p.exec(e.movR32Imm(rCX, msrProbeGPReg), e.rdmsr())
			p.pdr()
		}
	})
	if err != nil {
		return [4]uint32{}, err
	}
	return [4]uint32{uint32(v[0]), uint32(v[1]), uint32(v[2]), uint32(v[3])}, nil
}

// pagingState collects the paging setup of the stopped thread.
func (t *Thread) pagingState() (paging, error) {
	r, err := t.ReadRegisters([]string{"cr0", "cr3", "cr4", "efer"})
	if err != nil {
		return paging{}, err
	}
	pg := paging{cr0: r[0], cr3: r[1], cr4: r[2], efer: r[3]}
	ext, err := t.cpuid(0x80000008, 0)
	if err != nil {
		return paging{}, err
	}
	std, err := t.cpuid(1, 0)
	if err != nil {
		return paging{}, err
	}
	extFeat, err := t.cpuid(0x80000001, 0)
	if err != nil {
		return paging{}, err
	}
	pg.maxPhys = uint(ext[0] & 0xff)
	pg.pat, pg.pse36 = std[3]&(1<<16) != 0, std[3]&(1<<17) != 0
	pg.gbPages = extFeat[3]&(1<<26) != 0
	// EPT applies to a VMX guest whose EPT pointer has EPT enabled (the
	// pointer is saved by the switch to the flat environment).
	if t.entry&entryPostVMXON != 0 && t.scp[6]&scp6VMXGuest != 0 && t.flat.eptpOK && t.flat.eptp&eptpEnable != 0 {
		pg.ept, pg.eptp = true, t.flat.eptp
		if pg.eptCap, err = t.ReadMSR(msrVMXEPTVPIDCap); err != nil {
			return paging{}, err
		}
	}
	return pg, nil
}

// Translate maps a linear address to physical using the program's paging
// setup: none, 32-bit (with 4 MiB pages), PAE, 4-level or 5-level, and EPT
// for a VMX guest.
func (t *Thread) Translate(lin uint64) (uint64, error) {
	pg, err := t.pagingState()
	if err != nil {
		return 0, err
	}
	return pg.walk(lin, t.ReadMemory)
}

func bits(v uint64, hi, lo uint) uint64 { return v >> lo & (1<<(hi-lo+1) - 1) }

// addr returns entry bits [maxPhys-1:lo], in place.
func (pg paging) addr(entry uint64, lo uint) uint64 { return bits(entry, pg.maxPhys-1, lo) << lo }

// reserved reports whether any of entry's bits [hi:lo] is set (false if
// the range is empty).
func reserved(entry uint64, hi, lo uint) bool { return hi >= lo && bits(entry, hi, lo) != 0 }

// Entry kinds for the validity checks.
const (
	kPML5E = iota
	kPML4E
	kPDPTE
	kPDE
	kPTE
)

// valid32 checks a 32-bit paging entry (OpenIPC's
// ValidPageMode32StructureEntry).
func (pg paging) valid32(e uint64, kind int) bool {
	if e&1 == 0 {
		return false
	}
	if pg.cr4&(1<<4) == 0 {
		return true
	}
	if kind == kPTE {
		return pg.pat || e&(1<<7) == 0
	}
	if e&(1<<7) != 0 {
		if !pg.pse36 {
			if reserved(e, 21, 13) {
				return false
			}
		} else if reserved(e, 21, min(40, pg.maxPhys)-19) {
			return false
		}
		if !pg.pat && e&(1<<12) != 0 {
			return false
		}
	}
	return true
}

// validPAE checks a PAE paging entry (OpenIPC's
// ValidPageModePAEStructureEntry); large is set for a 2 MiB PDE.
func (pg paging) validPAE(e uint64, kind int, large bool) bool {
	if e&1 == 0 {
		return false
	}
	if kind == kPDPTE {
		return !reserved(e, 2, 1) && !reserved(e, 8, 5) && !reserved(e, 63, pg.maxPhys)
	}
	if reserved(e, 62, pg.maxPhys) || pg.efer&(1<<11) == 0 && e&(1<<63) != 0 {
		return false
	}
	switch {
	case kind == kPTE:
		return pg.pat || e&(1<<7) == 0
	case large:
		return !reserved(e, 20, 13) && (pg.pat || e&(1<<12) == 0)
	}
	return true
}

// valid64 checks a 4-/5-level paging entry (OpenIPC's
// ValidPageMode64StructureEntry).
func (pg paging) valid64(e uint64, kind int) bool {
	if e&1 == 0 {
		return false
	}
	if pg.maxPhys < 52 && reserved(e, 51, pg.maxPhys) {
		return false
	}
	if pg.efer&(1<<11) == 0 && e&(1<<63) != 0 {
		return false
	}
	ps := e&(1<<7) != 0
	switch kind {
	case kPML5E, kPML4E:
		return !ps
	case kPDPTE:
		return !ps || pg.gbPages && !reserved(e, 29, 13)
	case kPDE:
		return !ps || !reserved(e, 20, 13)
	}
	return true
}

// walk translates lin through the page tables described by pg, reading
// table entries (physical, or guest-physical with EPT) with read.
func (pg paging) walk(lin uint64, read func(uint64, []byte) error) (uint64, error) {
	phys := func(a uint64) (uint64, error) {
		if !pg.ept {
			return a, nil
		}
		return pg.eptWalk(a, read)
	}
	entry := func(a uint64, size int) (uint64, error) {
		pa, err := phys(a)
		if err != nil {
			return 0, err
		}
		var b [8]byte
		if err := read(pa, b[:size]); err != nil {
			return 0, err
		}
		return binary.LittleEndian.Uint64(b[:]), nil
	}
	bad := func(what string, e uint64) error {
		return fmt.Errorf("bigcore: %#x cannot be translated: %s %#x is not valid", lin, what, e)
	}
	if pg.cr0&(1<<31) == 0 {
		return phys(lin)
	}
	if pg.cr4&(1<<5) == 0 {
		// 32-bit paging: two levels of 4-byte entries.
		pde, err := entry(pg.cr3&0xfffff000|bits(lin, 31, 22)<<2, 4)
		if err != nil {
			return 0, err
		}
		if !pg.valid32(pde, kPDE) {
			return 0, bad("PDE", pde)
		}
		if pg.cr4&(1<<4) != 0 && pde&(1<<7) != 0 {
			// 4 MiB page; PDE bits 20:13 are physical address bits 39:32.
			return phys(bits(pde, 20, 13)<<32 | bits(pde, 31, 22)<<22 | bits(lin, 21, 0))
		}
		pte, err := entry(bits(pde, 31, 12)<<12|bits(lin, 21, 12)<<2, 4)
		if err != nil {
			return 0, err
		}
		if !pg.valid32(pte, kPTE) {
			return 0, bad("PTE", pte)
		}
		return phys(bits(pte, 31, 12)<<12 | bits(lin, 11, 0))
	}
	if pg.efer&(1<<8) == 0 {
		// PAE: 4-entry PDPT at CR3[31:5].
		pdpte, err := entry(pg.cr3&0xffffffe0+bits(lin, 31, 30)*8, 8)
		if err != nil {
			return 0, err
		}
		if !pg.validPAE(pdpte, kPDPTE, false) {
			return 0, bad("PDPTE", pdpte)
		}
		pde, err := entry(pg.addr(pdpte, 12)|bits(lin, 29, 21)<<3, 8)
		if err != nil {
			return 0, err
		}
		if pde&(1<<7) != 0 {
			if !pg.validPAE(pde, kPDE, true) {
				return 0, bad("PDE", pde)
			}
			return phys(pg.addr(pde, 21) | bits(lin, 20, 0))
		}
		if !pg.validPAE(pde, kPDE, false) {
			return 0, bad("PDE", pde)
		}
		pte, err := entry(pg.addr(pde, 12)|bits(lin, 20, 12)<<3, 8)
		if err != nil {
			return 0, err
		}
		if !pg.validPAE(pte, kPTE, false) {
			return 0, bad("PTE", pte)
		}
		return phys(pg.addr(pte, 12) | bits(lin, 11, 0))
	}

	// 4-level, or 5-level with CR4.LA57.
	table := pg.addr(pg.cr3, 12)
	if pg.cr4&(1<<12) != 0 {
		pml5e, err := entry(table|bits(lin, 56, 48)<<3, 8)
		if err != nil {
			return 0, err
		}
		if !pg.valid64(pml5e, kPML5E) {
			return 0, bad("PML5E", pml5e)
		}
		table = pg.addr(pml5e, 12)
	}
	pml4e, err := entry(table|bits(lin, 47, 39)<<3, 8)
	if err != nil {
		return 0, err
	}
	if !pg.valid64(pml4e, kPML4E) {
		return 0, bad("PML4E", pml4e)
	}
	pdpte, err := entry(pg.addr(pml4e, 12)|bits(lin, 38, 30)<<3, 8)
	if err != nil {
		return 0, err
	}
	if !pg.valid64(pdpte, kPDPTE) {
		return 0, bad("PDPTE", pdpte)
	}
	if pdpte&(1<<7) != 0 {
		return phys(pg.addr(pdpte, 30) | bits(lin, 29, 0))
	}
	pde, err := entry(pg.addr(pdpte, 12)|bits(lin, 29, 21)<<3, 8)
	if err != nil {
		return 0, err
	}
	if !pg.valid64(pde, kPDE) {
		return 0, bad("PDE", pde)
	}
	if pde&(1<<7) != 0 {
		return phys(pg.addr(pde, 21) | bits(lin, 20, 0))
	}
	pte, err := entry(pg.addr(pde, 12)|bits(lin, 20, 12)<<3, 8)
	if err != nil {
		return 0, err
	}
	if !pg.valid64(pte, kPTE) {
		return 0, bad("PTE", pte)
	}
	return phys(pg.addr(pte, 12) | bits(lin, 11, 0))
}

// EPT entry kinds.
const (
	eptNonLeaf = iota
	eptPDPTE1G
	eptPDE2M
	eptPTE
)

// validEPT checks an EPT entry (OpenIPC's ValidEptStructureEntry): readable,
// no reserved address bits, bits 7:3 clear in non-leaf entries, aligned
// large pages, and a valid memory type in leaf entries.
func (pg paging) validEPT(e uint64, kind int) bool {
	if e&1 == 0 {
		return false
	}
	if pg.maxPhys < 52 && reserved(e, 51, pg.maxPhys) {
		return false
	}
	switch kind {
	case eptNonLeaf:
		return !reserved(e, 7, 3)
	case eptPDPTE1G:
		if reserved(e, 29, 12) {
			return false
		}
	case eptPDE2M:
		if reserved(e, 20, 12) {
			return false
		}
	}
	mt := bits(e, 5, 3)
	return mt != 2 && mt != 3 && mt != 7
}

// eptWalk translates a guest-physical address through the EPT tables.
func (pg paging) eptWalk(gpa uint64, read func(uint64, []byte) error) (uint64, error) {
	entry := func(a uint64) (uint64, error) {
		var b [8]byte
		if err := read(a, b[:]); err != nil {
			return 0, err
		}
		return binary.LittleEndian.Uint64(b[:]), nil
	}
	bad := func(what string, e uint64) error {
		return fmt.Errorf("bigcore: guest-physical %#x cannot be translated: EPT %s %#x is not valid", gpa, what, e)
	}
	table := pg.addr(pg.eptp, 12)
	switch bits(pg.eptp, 5, 3) + 1 {
	case 4:
	case 5:
		if pg.eptCap&(1<<7) == 0 {
			return 0, fmt.Errorf("bigcore: EPT pointer asks for a 5-level walk, which IA32_VMX_EPT_VPID_CAP doesn't support")
		}
		pml5e, err := entry(table | bits(gpa, 56, 48)<<3)
		if err != nil {
			return 0, err
		}
		if !pg.validEPT(pml5e, eptNonLeaf) {
			return 0, bad("PML5E", pml5e)
		}
		table = pg.addr(pml5e, 12)
	default:
		return 0, fmt.Errorf("bigcore: EPT pointer %#x has an unsupported page-walk length", pg.eptp)
	}
	pml4e, err := entry(table | bits(gpa, 47, 39)<<3)
	if err != nil {
		return 0, err
	}
	if !pg.validEPT(pml4e, eptNonLeaf) {
		return 0, bad("PML4E", pml4e)
	}
	pdpte, err := entry(pg.addr(pml4e, 12) | bits(gpa, 38, 30)<<3)
	if err != nil {
		return 0, err
	}
	if pg.eptCap&(1<<17) != 0 && pdpte&(1<<7) != 0 {
		if !pg.validEPT(pdpte, eptPDPTE1G) {
			return 0, bad("1 GiB PDPTE", pdpte)
		}
		return pg.addr(pdpte, 30) | bits(gpa, 29, 0), nil
	}
	if !pg.validEPT(pdpte, eptNonLeaf) {
		return 0, bad("PDPTE", pdpte)
	}
	pde, err := entry(pg.addr(pdpte, 12) | bits(gpa, 29, 21)<<3)
	if err != nil {
		return 0, err
	}
	if pg.eptCap&(1<<16) != 0 && pde&(1<<7) != 0 {
		if !pg.validEPT(pde, eptPDE2M) {
			return 0, bad("2 MiB PDE", pde)
		}
		return pg.addr(pde, 21) | bits(gpa, 20, 0), nil
	}
	if !pg.validEPT(pde, eptNonLeaf) {
		return 0, bad("PDE", pde)
	}
	pte, err := entry(pg.addr(pde, 12) | bits(gpa, 20, 12)<<3)
	if err != nil {
		return 0, err
	}
	if !pg.validEPT(pte, eptPTE) {
		return 0, bad("PTE", pte)
	}
	return pg.addr(pte, 12) | bits(gpa, 11, 0), nil
}

// ReadLinear reads memory at a linear address (page by page).
func (t *Thread) ReadLinear(lin uint64, buf []byte) error {
	return t.linear(lin, buf, t.ReadMemory)
}

// WriteLinear writes memory at a linear address.
func (t *Thread) WriteLinear(lin uint64, buf []byte) error {
	return t.linear(lin, buf, t.WriteMemory)
}

func (t *Thread) linear(lin uint64, buf []byte, op func(uint64, []byte) error) error {
	pg, err := t.pagingState()
	if err != nil {
		return err
	}
	for off := 0; off < len(buf); {
		a := lin + uint64(off)
		n := min(len(buf)-off, int(0x1000-a&0xfff))
		pa, err := pg.walk(a, t.ReadMemory)
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
