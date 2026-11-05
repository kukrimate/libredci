// SPDX-License-Identifier: GPL-2.0-only
package bigcore

import (
	"encoding/binary"
	"fmt"
	"testing"
)

type fakeMem map[uint64][]byte

func (m fakeMem) put32(a uint64, v uint32) { m[a] = binary.LittleEndian.AppendUint32(nil, v) }
func (m fakeMem) put64(a uint64, v uint64) { m[a] = binary.LittleEndian.AppendUint64(nil, v) }
func (m fakeMem) read(a uint64, b []byte) error {
	v, ok := m[a]
	if !ok {
		v = make([]byte, len(b))
	}
	if len(v) != len(b) {
		return fmt.Errorf("bad read %#x/%d", a, len(b))
	}
	copy(b, v)
	return nil
}

func TestWalk(t *testing.T) {
	const (
		pg   = 1 << 31
		pse  = 1 << 4
		pae  = 1 << 5
		la57 = 1 << 12
		lma  = 1 << 10
	)
	m := fakeMem{}
	// 32-bit: 0x00401234 via PDE 1 -> PT at 0x3000, PTE 1 -> 0x55000.
	m.put32(0x2000+1*4, 0x3000|1)
	m.put32(0x3000+1*4, 0x55000|1)
	// 32-bit 4 MiB page: PDE 2 -> 0x01c00000 with PA[39:32] = 1.
	m.put32(0x2000+2*4, 0x01c00000|1<<13|1<<7|1)
	// PAE: PDPT at 0x4020, entry 1 -> PD 0x5000, PDE 3 -> 2 MiB page 0x40000000.
	m.put64(0x4020+1*8, 0x5000|1)
	m.put64(0x5000+3*8, 0x40000000|1<<7|1)
	// 4-level: PML4 0x6000[1] -> 0x7000[2] -> 0x8000[3] -> 0x9000[4] -> 0x1234000.
	m.put64(0x6000+1*8, 0x7000|1)
	m.put64(0x7000+2*8, 0x8000|1)
	m.put64(0x8000+3*8, 0x9000|1)
	m.put64(0x9000+4*8, 0x8000001234000|1)
	// 4-level, 1 GiB page in PDPT entry 5.
	m.put64(0x7000+5*8, 0x80000000|1<<7|1)
	// 5-level: PML5 0xa000[1] -> PML4 0x6000.
	m.put64(0xa000+1*8, 0x6000|1)

	lin4 := uint64(1)<<39 | 2<<30 | 3<<21 | 4<<12 | 0x567
	for _, c := range []struct {
		lin, cr0, cr3, cr4, efer, want uint64
		bad                            bool
	}{
		{lin: 0x12345678, want: 0x12345678},
		{lin: 0x00401234, cr0: pg, cr3: 0x2000, want: 0x55234},
		{lin: 0x00801234, cr0: pg, cr3: 0x2000, cr4: pse, want: 0x101c01234},
		{lin: 0x00c01234, cr0: pg, cr3: 0x2000, bad: true},
		{lin: 1<<30 | 3<<21 | 0x12345, cr0: pg, cr3: 0x4020, cr4: pae, want: 0x40012345},
		{lin: lin4, cr0: pg, cr3: 0x6000, cr4: pae, efer: lma, want: 0x8000001234567},
		{lin: 1<<39 | 5<<30 | 0x1234567, cr0: pg, cr3: 0x6000, cr4: pae, efer: lma, want: 0x81234567},
		{lin: 1<<48 | lin4, cr0: pg, cr3: 0xa000, cr4: pae | la57, efer: lma, want: 0x8000001234567},
	} {
		got, err := walk(c.lin, c.cr0, c.cr3, c.cr4, c.efer, m.read)
		if c.bad {
			if err == nil {
				t.Errorf("%#x: want error, got %#x", c.lin, got)
			}
			continue
		}
		if err != nil || got != c.want {
			t.Errorf("%#x: got %#x, %v; want %#x", c.lin, got, err, c.want)
		}
	}
}
