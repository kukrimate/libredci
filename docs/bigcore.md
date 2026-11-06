# Big-core run control (Skylake/Kaby Lake)

Big-core debugging requires the platform firmware to set
`IA32_DEBUG_INTERFACE` (MSR `0xc80`) bit 0 before the first SMI, which
locks the MSR.

Core TAP registers (see [models.md](models.md)):

| IR | DR bits | Function |
|---|---|---|
| `0x60` | 37 | `tap_status`: bit 6/5 probe-mode ready thread 0/1, bit 14/13 thread disabled |
| `0x83` | 134 | `sub_pir_write`: [131:4] 16 bytes of x86 code, [132] thread |
| `0x84`, `0x85` | 66 | probe data register (PDR) of thread 0 / 1, data in [64:1] |
| `0x86` | 4 | `probe_mode`: bit 1 = probe-mode entry request |

## Entry and exit

1. In the uncore's `pcode_wake_req`, set `probe_mode_pcode_flow` and the
   core's bit in `core_preq_enable`.
2. Write 2 to the core's `probe_mode` register. Both threads of the core
   stop; wait for their probe-mode ready bits in `tap_status`.
3. Exit: write 0 to `probe_mode`, then restore `pcode_wake_req`.

(PREQ via the DCI pins, or the uncore's parallel-core instructions `0x158`/
`0x159`, are equivalent ways to request entry and exit.)

## Executing instructions

The core executes x86 instructions written through `sub_pir_write`, one
per scan (the rest of the 16 bytes filled with `nop`), **in the halted
thread's context and current mode** (16-, 32- or 64-bit), so encodings must
match that mode. After each scan the host waits for completion (probe-mode
ready); over DCI this is a trigger wait (see [protocol.md](protocol.md)).

Special MSR indices are available in probe mode:

| `rdmsr` with ECX = | Effect |
|---|---|
| `0x70` | EAX = index: copies the state element (e.g. scratch register `0x160+n`) into the PDR |
| `0x71` | copies EAX into the PDR |
| `0x72` | EAX = segment (ES 1, CS 3, SS 5, DS 7, FS 9, GS 11, IDT 12, GDT 13, TR 14, LDT 15): base into the PDR; limit, attributes and selector into scratch registers |
| `0x101` | current privilege level |
| `0x160`–`0x16f` | scratch registers SCP0–SCP15 |
| `0x1df` | probe-mode control (ICECTLPMR): bit 0 redirect debug exceptions into probe mode, bit 4 last instruction faulted, bit 14 guard the debug registers, bit 17 remain in probe mode |
| `0x77` | probe-mode paging control: 0 = paging off, `0x80000000` = on |
| `0x173` | staging register for segment state written through `0x72` |

On entry the microcode saves **RCX, RDX, RIP and RAX in SCP1, SCP3, SCP7 and
SCP8** and reloads them from there on exit; the stopped thread's other
registers are live. The first PDR read after entry returns a snapshot:
EFER [15:0], prior state [23:16], in-SMM [24], CS selector [47:32] and CS
attributes [63:48] (bit 13 L, bit 14 D), which determine the thread's mode.

## Reading state

* RAX, RCX, RDX, RIP: the saved SCP8, SCP1, SCP3, SCP7.
* Other general-purpose, control and debug registers: `mov rax, <reg>`, then
  `mov ecx, 0x71; rdmsr`, then read the PDR.
* RFLAGS: the saved part from SCP2, merged with the live arithmetic flags
  (`lahf`, `seto`).
* MSRs: clear `0x1df` bit 4, `rdmsr`; EAX via `0x71`; EDX via index
  `0x161` of `0x70`; then check `0x1df` bit 4 for a fault.

## State to save and restore

Before running its own instructions, the debugger saves SCP1–SCP10, RBX,
RFLAGS, the CPL (`0x101`), DR6, DR7 and `0x1df`, then sets CPL 0, clears
`0x1df` bit 14 and sets DR7 to `0x400`. Before exit it writes back, in order:
DR6 = `0xffff0ff0`, DR7, `0x1df` (with bit 17 clear), RFLAGS, CPL, RBX,
SCP1–SCP10.

## Memory and I/O

Memory and I/O instructions run in the thread's mode and with its paging,
so the debugger first switches the thread to a flat 64-bit environment and
undoes the switch before resuming:

1. Save SCP11–SCP13, RSI, RDI, CR3, CR4, EFER, the EPT pointer (MSR
   `0x510`) and the CS, SS, GDT, IDT, TR and LDT segment state.
2. Make SS a flat writable data segment.
3. If long mode isn't active: enter protected mode without paging with a
   16-bit CS and an empty TSS, then set CR4.PAE, EFER.LME and CR0.PG.
4. Set CS.L, flush the TLBs, clear CR0.TS, write 0 to `0x77` (paging off)
   and turn EPT off.

Addresses are then physical. Accesses are split into naturally sized
pieces of up to 4 bytes (`mov eax, [rdx]`, `mov [rdx], eax`; `in`/`out`
with DX for ports), and `0x1df` bit 4 is checked after each access for a
fault. An 8-byte load from some MMIO registers (on KBL-R, MCHBAR+0x8)
never completes: the core stops signalling probe-mode ready and the DCI
JTAG engine waits for it until the target is reset. Before exit,
everything is restored in reverse order.

Linear addresses are translated by walking the program's page tables
(32-bit, PAE, 4-level or 5-level paging) with physical reads.

## Breakpoints and single-step

* **Hardware breakpoints**: program DR0–DR3 and DR7 (as on Lakemont), set
  `0x1df` bit 0 so that #DB enters probe mode instead of the program's
  handler, and resume with RFLAGS.RF set. Setting `0x1df` bit 14 keeps the
  program from changing the debug registers meanwhile. A hit re-enters probe
  mode with DR6.Bn set.
* **Single-step**: resume with RFLAGS.TF and RF and `0x1df` bit 0 set.

The debug registers are live in probe mode: they are written with
`mov drN, rax` during the session, and DR7 must be set back from `0x400`
before exit. A thread re-entering probe mode through #DB does not provide a
fresh entry snapshot in the PDR; its mode follows from the CS attributes
(`0x72` with EAX = 3).

Probe-mode ready, and with it the trigger used to wait for instruction
completion, is only signalled once **both** threads of a core are in probe
mode. When one thread stops at a breakpoint, the debugger therefore halts
its sibling before executing instructions on it.

A thread in HLT stays in HLT after probe-mode exit; breakpoints only fire
once it wakes (for example on an interrupt).
