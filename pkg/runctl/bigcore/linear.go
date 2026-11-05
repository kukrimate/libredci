// SPDX-License-Identifier: GPL-2.0-only
package bigcore

import (
	"encoding/binary"
	"fmt"
)

// CodeMode is the operand mode (16, 32 or 64) the stopped thread executes in.
func (t *Thread) CodeMode() int { return t.entryMode() }

// Translate maps a linear address to physical using the program's paging
// setup: none, 32-bit (with 4 MiB pages), PAE, 4-level or 5-level.
func (t *Thread) Translate(lin uint64) (uint64, error) {
	r, err := t.ReadRegisters([]string{"cr0", "cr3", "cr4", "efer"})
	if err != nil {
		return 0, err
	}
	return walk(lin, r[0], r[1], r[2], r[3], t.ReadMemory)
}

// walk translates lin through the page tables described by the control
// registers, reading table entries with read.
func walk(lin, cr0, cr3, cr4, efer uint64, read func(uint64, []byte) error) (uint64, error) {
	entry32 := func(addr uint64) (uint32, error) {
		var b [4]byte
		if err := read(addr, b[:]); err != nil {
			return 0, err
		}
		return binary.LittleEndian.Uint32(b[:]), nil
	}
	if cr0&(1<<31) == 0 {
		return lin, nil
	}
	const (
		pse  = 1 << 4
		pae  = 1 << 5
		la57 = 1 << 12
		lma  = 1 << 10
	)
	if cr4&pae == 0 {
		// 32-bit paging: two levels of 4-byte entries.
		lin &= 0xffffffff
		pde, err := entry32(uint64(uint32(cr3)&^0xfff) + (lin>>22)*4)
		if err != nil {
			return 0, err
		}
		if pde&1 == 0 {
			return 0, fmt.Errorf("bigcore: %#x not mapped (PDE %#x)", lin, pde)
		}
		if pde&(1<<7) != 0 && cr4&pse != 0 {
			// 4 MiB page; PDE bits 20:13 are physical address bits 39:32.
			return uint64(pde&0xffc00000) | uint64(pde>>13&0xff)<<32 | lin&0x3fffff, nil
		}
		pte, err := entry32(uint64(pde&^0xfff) + (lin>>12&0x3ff)*4)
		if err != nil {
			return 0, err
		}
		if pte&1 == 0 {
			return 0, fmt.Errorf("bigcore: %#x not mapped (PTE %#x)", lin, pte)
		}
		return uint64(pte&^0xfff) | lin&0xfff, nil
	}

	// 8-byte entries. levels lists the shift of each level's index.
	const addrMask = 0x000ffffffffff000
	var levels []uint
	table := cr3 & addrMask
	switch {
	case efer&lma == 0: // PAE: 4-entry PDPT at CR3[31:5]
		lin &= 0xffffffff
		levels = []uint{30, 21, 12}
		table = cr3 & 0xffffffe0
	case cr4&la57 != 0:
		levels = []uint{48, 39, 30, 21, 12}
	default:
		levels = []uint{39, 30, 21, 12}
	}
	for i, shift := range levels {
		idx := lin >> shift & 0x1ff
		if i == 0 && efer&lma == 0 {
			idx = lin >> shift & 3
		}
		var b [8]byte
		if err := read(table+idx*8, b[:]); err != nil {
			return 0, err
		}
		ent := binary.LittleEndian.Uint64(b[:])
		if ent&1 == 0 {
			return 0, fmt.Errorf("bigcore: %#x not mapped (level %d entry %#x)", lin, len(levels)-i, ent)
		}
		last := shift == 12
		// Large pages: 1 GiB at shift 30, 2 MiB at shift 21 (not in a PAE PDPT).
		if !last && ent&(1<<7) != 0 && (shift == 21 || shift == 30 && efer&lma != 0) {
			size := uint64(1) << shift
			return ent&addrMask&^(size-1) | lin&(size-1), nil
		}
		if last {
			return ent&addrMask | lin&0xfff, nil
		}
		table = ent & addrMask
	}
	panic("unreachable")
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
	for off := 0; off < len(buf); {
		a := lin + uint64(off)
		n := min(len(buf)-off, int(0x1000-a&0xfff))
		pa, err := t.Translate(a)
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
