// SPDX-License-Identifier: GPL-2.0-only
package bigcore

import "encoding/binary"

// A tiny x86 encoder for the instructions used in probe mode. Instructions
// execute in the halted thread's current mode, so encodings depend on it.

// Register numbers (also used for r8 = al/cl/dl/bl/ah/ch/dh/bh).
const (
	rAX = iota
	rCX
	rDX
	rBX
	rSP
	rBP
	rSI
	rDI
	r8
	r9
	r10
	r11
	r12
	r13
	r14
	r15
)

const (
	rAL = 0
	rCL = 1
	rAH = 4
	rCH = 5
)

type enc struct {
	mode int // 16, 32 or 64
}

// op32 is the operand-size prefix needed for a 32-bit operation.
func (e enc) op32() []byte {
	if e.mode == 16 {
		return []byte{0x66}
	}
	return nil
}

func cat(parts ...[]byte) []byte {
	var out []byte
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

func le32(v uint32) []byte { b := make([]byte, 4); binary.LittleEndian.PutUint32(b, v); return b }
func le64(v uint64) []byte { b := make([]byte, 8); binary.LittleEndian.PutUint64(b, v); return b }

func (e enc) movR8Imm(r int, v byte) []byte { return []byte{0xb0 + byte(r), v} }

// movChCl: mov ch, cl.
func (e enc) movChCl() []byte { return []byte{0x88, 0xcd} }

// bswapECX32 swaps ecx as a 32-bit register; bswapCX16 is the 16-bit form
// (undefined result). Together with byte moves they clear ECX in any mode.
func (e enc) bswapECX32() []byte { return cat(e.op32(), []byte{0x0f, 0xc9}) }
func (e enc) bswapCX16() []byte {
	if e.mode == 16 {
		return []byte{0x0f, 0xc9}
	}
	return []byte{0x66, 0x0f, 0xc9}
}

func (e enc) movR32Imm(r int, v uint32) []byte {
	return cat(e.op32(), []byte{0xb8 + byte(r&7)}, le32(v))
}

func (e enc) movR64Imm(r int, v uint64) []byte {
	rex := byte(0x48)
	if r >= 8 {
		rex |= 0x01
	}
	return cat([]byte{rex, 0xb8 + byte(r&7)}, le64(v))
}

// movImm loads a full register-width value: 64-bit in 64-bit mode,
// otherwise 32-bit.
func (e enc) movImm(r int, v uint64) []byte {
	if e.mode == 64 {
		return e.movR64Imm(r, v)
	}
	return e.movR32Imm(r, uint32(v))
}

func modrm(reg, rm int) byte { return 0xc0 | byte(reg&7)<<3 | byte(rm&7) }

// movRR: mov dst, src at register width (64-bit in 64-bit mode).
func (e enc) movRR(dst, src int) []byte {
	if e.mode == 64 {
		rex := byte(0x48)
		if src >= 8 {
			rex |= 0x04
		}
		if dst >= 8 {
			rex |= 0x01
		}
		return []byte{rex, 0x89, modrm(src, dst)}
	}
	return cat(e.op32(), []byte{0x89, modrm(src, dst)})
}

// movFromCR: mov dst, crN.
func (e enc) movFromCR(dst, cr int) []byte {
	var p []byte
	if cr >= 8 {
		p = []byte{0x44}
	}
	return cat(p, []byte{0x0f, 0x20, modrm(cr, dst)})
}

func (e enc) movFromDR(dst, dr int) []byte { return []byte{0x0f, 0x21, modrm(dr, dst)} }
func (e enc) movToDR(dr, src int) []byte   { return []byte{0x0f, 0x23, modrm(dr, src)} }

func (e enc) rdmsr() []byte { return []byte{0x0f, 0x32} }
func (e enc) wrmsr() []byte { return []byte{0x0f, 0x30} }
func (e enc) lahf() []byte  { return []byte{0x9f} }
func (e enc) setoCL() []byte {
	return []byte{0x0f, 0x90, 0xc1}
}

// Register-width ALU helpers.
func (e enc) w() []byte {
	if e.mode == 64 {
		return []byte{0x48}
	}
	return e.op32()
}

func (e enc) shrImm(r int, n byte) []byte { return cat(e.w(), []byte{0xc1, 0xe8 | byte(r), n}) }
func (e enc) shlImm(r int, n byte) []byte { return cat(e.w(), []byte{0xc1, 0xe0 | byte(r), n}) }
func (e enc) orRR(dst, src int) []byte    { return cat(e.w(), []byte{0x09, modrm(src, dst)}) }

// andEAXImm: and eax, imm32 (32-bit operation in every mode).
func (e enc) andEAXImm(v uint32) []byte { return cat(e.op32(), []byte{0x25}, le32(v)) }

// movToCR: mov crN, src.
func (e enc) movToCR(cr, src int) []byte {
	var p []byte
	if cr >= 8 {
		p = []byte{0x44}
	}
	return cat(p, []byte{0x0f, 0x22, modrm(cr, src)})
}

// 32-bit ALU operations on EAX/EDX with an immediate (in 64-bit mode the
// result is zero-extended).
func (e enc) orEAXImm(v uint32) []byte  { return cat(e.op32(), []byte{0x0d}, le32(v)) }
func (e enc) xorEAXImm(v uint32) []byte { return cat(e.op32(), []byte{0x35}, le32(v)) }
func (e enc) andEDXImm(v uint32) []byte { return cat(e.op32(), []byte{0x81, 0xe2}, le32(v)) }

// andRAXImm: and rax, simm32 (64-bit mode only).
func (e enc) andRAXImm(v int32) []byte { return cat([]byte{0x48, 0x25}, le32(uint32(v))) }

// orECXtoEAX: or eax, ecx (32-bit).
func (e enc) orEAXECX() []byte { return cat(e.op32(), []byte{0x09, 0xc8}) }

// Memory access through [rdx] in 64-bit mode. size is 1, 2, 4 or 8.
func (e enc) loadRDX(size int) []byte {
	switch size {
	case 1:
		return []byte{0x0f, 0xb6, 0x02} // movzx eax, byte [rdx]
	case 2:
		return []byte{0x0f, 0xb7, 0x02} // movzx eax, word [rdx]
	case 4:
		return []byte{0x8b, 0x02} // mov eax, [rdx]
	default:
		return []byte{0x48, 0x8b, 0x02} // mov rax, [rdx]
	}
}

func (e enc) storeRDX(size int) []byte {
	switch size {
	case 1:
		return []byte{0x88, 0x02} // mov [rdx], al
	case 2:
		return []byte{0x66, 0x89, 0x02} // mov [rdx], ax
	case 4:
		return []byte{0x89, 0x02} // mov [rdx], eax
	default:
		return []byte{0x48, 0x89, 0x02} // mov [rdx], rax
	}
}

// Port I/O through dx in 64-bit mode.
func (e enc) in(size int) []byte {
	switch size {
	case 1:
		return []byte{0xec}
	case 2:
		return []byte{0x66, 0xed}
	default:
		return []byte{0xed}
	}
}

func (e enc) out(size int) []byte {
	switch size {
	case 1:
		return []byte{0xee}
	case 2:
		return []byte{0x66, 0xef}
	default:
		return []byte{0xef}
	}
}
