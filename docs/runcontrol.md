# Run control

`pkg/runctl` provides architecture-independent run control: a `Thread` can be
halted and resumed, and its registers, physical memory and I/O ports
accessed. Optional interfaces add MSRs, hardware breakpoints and
single-stepping. Platform models declare the threads on each TAP; an
implementation is selected by the thread's architecture.

| Architecture | Threads | Package |
|---|---|---|
| `lakemont` | CSME and ISH cores of SPT (`csme` on `pch.left.csme_par.csme`, `ish` on `pch.lb.ish_par.ish`) | `pkg/runctl/lakemont` |
| `kbl-core` | Kaby Lake cores (`cpu.c<core>t<thread>`) | `pkg/runctl/bigcore` |

Big-core debugging requires the platform firmware to set
`IA32_DEBUG_INTERFACE` (MSR `0xc80`) bit 0 before the first SMI, which
locks the MSR.

## Lakemont probe mode

Lakemont-class cores (Intel Quark, the CSME and ISH in Intel PCHs) are debugged
through probe mode on their TAP.

| IR | DR bits | Function |
|---|---|---|
| `0x03` | — | Execute the probe instruction in the PIR (passing Update-IR is enough) |
| `0x04` | 1 | Probe-mode request: 1 = enter, 0 = leave |
| `0x06` | 64 | Probe instruction register (PIR) |
| `0x08` | 32 | Probe data register (PDR), read/write |
| `0x0b` | 32 | Status |
| `0x35` | 1 | Reset break: stop at the reset vector on the next reset |

Status register bits:

| Bit | Meaning |
|---|---|
| 0 | PRDY |
| 1 | Probe-mode request pending |
| 2 | In probe mode |
| 3 | Probe mode entered by redirection (debug exception) |
| 4 | Software breakpoints supported |
| 5, 6 | Shutdown break enabled / occurred |
| 7, 8 | Machine-check break enabled / occurred |
| 9 | Memory operation in progress |
| 12:10 | Probe-mode state machine |
| 18 | Stopped by reset break |

### Probe instructions

A probe instruction is up to 8 bytes of x86 machine code, **each byte
bit-reversed**, packed low byte first and padded with NOPs (`0x90`, which is
`0x09` reversed). For example `rdmsr` (`0f 32`) is `0x0909090909094cf0`, and
`mov edx, [eax]` (`8b 10`) is `0x09090909090908d1`. `lakemont.PIR()` builds
them.

Registers are held in a shadow RAM while the core is in probe mode:

* **Read**: execute the register's select instruction, then `0x0000000e9d660000`
  (SRAM access), then `0x4cf0000000000000` (SRAM → PDR), then scan the PDR.
* **Write**: execute the select instruction and SRAM access, scan the value
  into the PDR, then execute `0x0cf0000000000000` (PDR → SRAM).

Writes take effect when the core leaves probe mode. The select instructions
for all registers (general purpose, EIP, EFLAGS, segment selectors, bases,
limits and attributes, control, debug, descriptor-table registers, PMCR) are
listed in `pkg/runctl/lakemont`.

### Memory, I/O and MSRs

| Operation | Inputs | Instruction | Result |
|---|---|---|---|
| Memory read | EAX = address | `mov dl/dx/edx, [eax]` | EDX |
| Memory write | EAX = address, EDX = data | `mov [eax], dl/dx/edx` | — |
| I/O read | EDX = port | `in al/ax/eax, dx` | EAX |
| I/O write | EDX = port, EAX = data | `out dx, al/ax/eax` | — |
| MSR read | ECX = index | `rdmsr` | EDX:EAX |
| MSR write | ECX = index, EDX:EAX = value | `wrmsr` | — |

After any of these, **wait about 1 ms before the next probe-register
access.** If the next probe instruction arrives while the operation is still
running, the core hangs (status `0x80a12` on SPT) until the platform is reset.

MSR access requires CS.RPL = 0; some Lakemont versions crash otherwise.
Accessing an MSR the core doesn't implement faults inside probe mode, and on
SPT this powers the platform off. Verified to exist on the SPT CSME:
`0x10` (TSC), `0x1d9` (DEBUGCTL).

Memory accesses use the current DS and paging setup. The halt procedure below
makes them physical accesses.

### Halt

1. Write probe-mode request = 1. Poll status until bit 2 is set and bit 1 is clear.
2. Save EFLAGS, CS base/limit/AR, SS AR, DS base/limit/AR, CR0 and DR7.
3. Set up a flat environment: DS and CS base 0, limit `0xffffffff`, AR
   `0x004f9300`; SS AR `0x004f9300`; DR7 = 0; EFLAGS.IF and VM cleared; CR0
   with PG cleared, then with CD and NW set (two writes). Do not issue WBINVD
   on the SPT CSME: it leaves the core busy.
4. DR6 gives the stop reason. EIP (plus CS base) gives the location.

Save EAX, ECX and EDX before using them for memory, I/O or MSR accesses.

### Resume

Write all saved values back, then write probe-mode request = 0 and wait for
status bit 2 to clear.

The CSME firmware runs with non-flat segments. Resuming it with the flat
debug setup still in place crashes the ME and, on SPT, the platform. libredci
therefore:

* keeps the saved values in `$XDG_CACHE_HOME/libredci/lakemont-<thread>.json`
  while the core is halted, so a later session can `attach` and resume
  correctly;
* refuses to resume a core whose pre-halt state it doesn't know (`forcego`
  overrides);
* restores everything and leaves the core halted if the halt setup fails.

### Breakpoints and single-step

PMCR, a probe-mode register, controls which events redirect into probe mode:
bit 0 debug exceptions (#DB), bit 1 shutdown, bit 2 machine check.

* **Hardware breakpoints**: program DR0–DR3 with linear addresses and DR7 with
  local-enable and RW/LEN bits (RW: 0 execute, 1 write, 2 I/O, 3 read/write),
  set PMCR bit 0, and resume with EFLAGS.RF set. A hit re-enters probe mode
  with the matching DR6.Bn bit set.
* **Single-step**: resume with EFLAGS.TF and RF and PMCR bit 0 set. The core
  re-enters probe mode after one instruction, with DR6.BS set.

A core resumed with breakpoints or stepping armed can stop again within
microseconds, so don't treat an immediate return to probe mode as an error.
libredci hides TF and the DR6 bits it caused from the program, and restores the
program's own DR7 and PMCR once no breakpoints remain.

### Reset break

Writing 1 to IR `0x35` makes the core stop in probe mode at its reset vector
on its next reset; status bit 18 is then set. The register doesn't read back
the written value. The state at a reset-break stop is the reset state, so it
can be resumed without restoring anything.

On SPT the CSME resets only with the platform (global reset or power cycle).
The DCI platform reset (see [protocol.md](protocol.md)) is a power cycle,
which doesn't preserve an armed reset break. In addition, a DbC link only
returns after host firmware has initialised xHCI, so reset break cannot catch
a CSME boot over DbC.

## Big-core probe mode (Skylake/Kaby Lake)

Core TAP registers (see [models.md](models.md)):

| IR | DR bits | Function |
|---|---|---|
| `0x60` | 37 | `tap_status`: bit 6/5 probe-mode ready thread 0/1, bit 14/13 thread disabled |
| `0x83` | 134 | `sub_pir_write`: [131:4] 16 bytes of x86 code, [132] thread |
| `0x84`, `0x85` | 66 | probe data register (PDR) of thread 0 / 1, data in [64:1] |
| `0x86` | 4 | `probe_mode`: bit 1 = probe-mode entry request |

### Entry and exit

1. In the uncore's `pcode_wake_req`, set `probe_mode_pcode_flow` and the
   core's bit in `core_preq_enable`.
2. Write 2 to the core's `probe_mode` register. Both threads of the core
   stop; wait for their probe-mode ready bits in `tap_status`.
3. Exit: write 0 to `probe_mode`, then restore `pcode_wake_req`.

(PREQ via the DCI pins, or the uncore's parallel-core instructions `0x158`/
`0x159`, are equivalent ways to request entry and exit.)

### Executing instructions

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

### Reading state

* RAX, RCX, RDX, RIP: the saved SCP8, SCP1, SCP3, SCP7.
* Other general-purpose, control and debug registers: `mov rax, <reg>`, then
  `mov ecx, 0x71; rdmsr`, then read the PDR.
* RFLAGS: the saved part from SCP2, merged with the live arithmetic flags
  (`lahf`, `seto`).
* MSRs: clear `0x1df` bit 4, `rdmsr`; EAX via `0x71`; EDX via index
  `0x161` of `0x70`; then check `0x1df` bit 4 for a fault.

### State to save and restore

Before running its own instructions, the debugger saves SCP1–SCP10, RBX,
RFLAGS, the CPL (`0x101`), DR6, DR7 and `0x1df`, then sets CPL 0, clears
`0x1df` bit 14 and sets DR7 to `0x400`. Before exit it writes back, in order:
DR6 = `0xffff0ff0`, DR7, `0x1df` (with bit 17 clear), RFLAGS, CPL, RBX,
SCP1–SCP10.

### Memory and I/O

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
pieces of up to 8 bytes (`mov eax/rax, [rdx]`, `mov [rdx], eax/rax`;
`in`/`out` with DX for ports), and `0x1df` bit 4 is checked after each
access for a fault. Before exit, everything is restored in reverse order.

Linear addresses are translated by walking the program's page tables
(32-bit, PAE, 4-level or 5-level paging) with physical reads.

### Breakpoints and single-step

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
