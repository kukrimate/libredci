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

The sequences are OpenIPC's (captured with the libusb shim). Writes to
`pcode_wake_req` are read-modify-writes in one split DR scan (see
[models.md](models.md#registers)), so the other fields keep their values.

Halt:

1. Read the core's `tap_status`.
2. In the uncore's `pcode_wake_req`, set `probe_mode_pcode_flow` and set
   `core_preq_enable` to only this core's bit.
3. Assert PREQ (bank-1 register `0x05` = `0x3`). Both threads of the core
   enter probe mode; wait for their probe-mode ready bits in `tap_status`.
4. Save the threads' state (enabling trigger mode for instruction waits).
5. Deassert PREQ (`0x05` = `0x1`), set `core_preq_enable` back to `0xff`
   (all cores) and release TAP arbitration.

Resume:

1. Restore the threads' state.
2. Deassert PREQ and set `core_preq_enable` to `0xff`. With breakpoints or
   single-step armed, then set `probe_mode_pcode_flow` and limit
   `core_preq_enable` to this core: a debug exception redirected into probe
   mode stops the cores enabled there, so it stops only this core.
3. Write 0 to the core's `probe_mode` register and release TAP arbitration.

Any number of cores can be halted at the same time; each is halted and
resumed on its own. `probe_mode_pcode_flow` is not cleared: after a halt
it reads 1 (and pcode sets `turbo_disable` and `freeze_ia_freq` while a
core is in probe mode).

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

* RAX, RCX, RDX, RIP: the saved SCP8, SCP1, SCP3, SCP7. CR0: the saved
  SCP9.
* RBX, RSI, RDI, DR6, DR7: `mov rax, <reg>`, then `mov ecx, 0x71; rdmsr`,
  then read the PDR, in the thread's own mode.
* RSP, RBP, R8–R15, CR2, CR3, CR4, CR8, DR0–DR3 and segment bases: the
  same, but only after the switch to the flat 64-bit environment (see
  "Memory and I/O"), as OpenIPC does, so a thread in compatibility mode
  gives full 64-bit values. R8–R15 and CR8 exist whenever long mode is
  active.
* RFLAGS: the saved part from SCP2, merged with the live arithmetic flags
  (`lahf`, `seto`).
* MSRs: clear `0x1df` bit 4, `rdmsr`; EAX via `0x71`; EDX via index
  `0x161` of `0x70`; then check `0x1df` bit 4 for a fault. IA32_PEBS_ENABLE
  (`0x3f1`) holds the probe-mode value; the program's value is in SCP13,
  which is read and written instead.

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

1. Save SCP11–SCP13, CR4, EFER, the EPT pointer (MSR `0x510`) and the
   CS, SS, GDT, IDT, TR and LDT segment state.
2. Make SS a flat writable data segment.
3. If long mode isn't active: enter protected mode without paging with a
   16-bit CS and an empty TSS, then set CR4.PAE, EFER.LME and CR0.PG.
4. Set CS.L, flush the TLBs, clear CR0.TS, write 0 to `0x77` (paging off)
   and turn EPT off.
5. Save RSI, RDI and CR3 (in full, with 64-bit encodings).

Addresses are then physical. Accesses are split into naturally sized
pieces of up to 4 bytes (`mov eax, [rdx]`, `mov [rdx], eax`; `in`/`out`
with DX for ports), and `0x1df` bit 4 is checked after each access for a
fault. An 8-byte load from some MMIO registers (on KBL-R, MCHBAR+0x8)
never completes: the core stops signalling probe-mode ready and the DCI
JTAG engine waits for it until the target is reset. Before exit,
everything is restored in reverse order.

Linear addresses are translated by walking the program's page tables
(32-bit, PAE, 4-level or 5-level paging) with physical reads, following
OpenIPC's `linear_to_physical`: 4-level paging is chosen by EFER.LME, table
addresses are limited to the physical address width (CPUID
`0x80000008`), and each entry is checked as OpenIPC does (present;
reserved address bits; NX only with EFER.NXE; PS not allowed in PML5E/PML4E
and in PDPTEs without 1 GiB page support; reserved bits in large-page
entries; the PAT bit only with PAT support). CPUID runs once per register
read, as in OpenIPC. For a thread in VMX guest mode (entry snapshot bit 27
and SCP6 bit 62) with EPT enabled (EPT pointer bit 63), every table address
and the result are guest-physical and are translated through the EPT tables
(4 or 5 levels, 1 GiB/2 MiB pages per IA32_VMX_EPT_VPID_CAP, entries
checked for read access, reserved bits and memory type).

## Breakpoints and single-step

* **Hardware breakpoints**: program DR0–DR3 and DR7 (as on Lakemont), set
  `0x1df` bit 0 so that #DB enters probe mode instead of the program's
  handler, and resume with RFLAGS.RF set. Setting `0x1df` bit 14 keeps the
  program from changing the debug registers meanwhile. A hit re-enters probe
  mode with DR6.Bn set.
* **Single-step**: resume with RFLAGS.TF and RF and `0x1df` bit 0 set, and
  with IA32_DEBUGCTL.BTF (MSR `0x1d9` bit 1) clear so that the step is one
  instruction; the program's BTF is put back after the step. Two step
  modes follow OpenIPC's `StepMode` setting (`stepmode` in the shell):
  * `targeted` (the default; OpenIPC's `ReleaseTargetedThread`): only the
    stepped thread runs. The sibling's state is saved and its `0x1df` bit
    17 (remain in probe mode) is set before `probe_mode` is cleared, so it
    stays in probe mode; the next full resume restores it with bit 17
    clear. Other cores are not affected.
  * `all` (OpenIPC's default on KBL, `ReleaseAllThreads`): every held core
    is restored, `core_preq_enable` is set to `0xff` with the pcode flow
    bit, and all cores leave probe mode at once through the uncore's
    parallel scans (IR `0x158` loads `0x86` into every core TAP, IR `0x159`
    writes 0). The debug exception after the step stops every core; they
    stay held until resumed (`go` on a thread resumes its core; leaving the
    shell resumes all held cores).
* **I/O breakpoints** (DR7 type 2) need CR4.DE: it is set while one is
  armed and returned to the program's value once none is.

The debug registers are live in probe mode: they are written with
`mov drN, rax` during the session, and DR7 must be set back from `0x400`
before exit. DR0–DR3 are written in the flat 64-bit environment. A slot's
program value is saved when the debugger first uses it; the slot gets that
value back on resume once its breakpoint is cleared, and `reg drN` shows
the program's value for it. A thread re-entering probe mode through #DB
provides a fresh entry snapshot (prior state `0x01`). If a snapshot isn't
plausible, the mode is taken from the CS attributes (`0x72` with EAX = 3).

Probe-mode ready, and with it the trigger used to wait for instruction
completion, is only signalled once **both** threads of a core are in probe
mode. A debug exception on one thread stops the whole core (`tap_status`
`0x400166` for thread 0, `0x4000e6` for thread 1); the debugger checks
that both threads are in probe mode before executing instructions.

A thread in HLT stays in HLT after probe-mode exit; breakpoints only fire
once it wakes (for example on an interrupt). As in OpenIPC, a thread whose
prior state is HLT, MWAIT, wait for SIPI or error shutdown cannot be stepped
until its RIP differs from the stop address (it would not execute an
instruction). A step that does not stop within a second is followed by a
halt; TF and RF are then removed from the saved RFLAGS and BTF is restored.

Stop reasons name the entry snapshot's prior state with OpenIPC's names
(`0x01` debug trap, `0x23` MWAIT C1 B, `0x88` HLT, `0x83` INIT, `0x04` SMM
entry, …).

## Sessions

Like OpenIPC after each operation, the shell drops the CPU wake requests
(`cpu_cores_awake`, `cpu_cbos_awake`, `cpu_package_awake`) and the uncore
TAP arbitration after every command; the next access acquires them again.
Halted cores stay in probe mode meanwhile.

Physical addresses between `0x800000000000` and `0xffff7fffffffffff` are
rejected unless the thread has CR4.LA57 set, as in OpenIPC.

## Differences from OpenIPC

The big-core sequences follow OpenIPC (captures, and its Python run
control in the decrypted `OpenIPC/Data`). These differences are deliberate:

* Memory accesses are at most 4 bytes; OpenIPC reads at the requested
  width (an 8-byte load from MCHBAR+0x8 never completes). The fault flag is
  checked after each batch of accesses.
* Removing a breakpoint gives the slot back the program's DRn; OpenIPC
  writes 0.
* Data breakpoints set the DR7 length bits; OpenIPC's procedure only sets
  the type.
* RFLAGS.TF is removed from the saved state only after the debugger's own
  steps; OpenIPC clears it at every state save.
* An entry snapshot that isn't plausible is replaced by the mode derived
  from the CS attributes; OpenIPC reports an error.
* The `targeted` step mode is the default; OpenIPC's default on KBL is
  `ReleaseAllThreads` (`stepmode all`).
* The PAE PDPTE is read at CR3[31:5] + 8 × index; OpenIPC's procedure
  computes `CR3[31:2] | index` (no scaling by 8).
* EPT entries with R = 0 are rejected, as OpenIPC's check does first; its
  later checks for R = 0 entries are unreachable.

OpenIPC features libredci doesn't have: software breakpoints (INT1, opcode
`0xf1`); break conditions (SMM entry/exit, INIT, shutdown, machine check,
VM launch/exit, debug register protection); branch stepping (BTF), step
over and step out; FP/SSE/AVX/MPX registers, XCR0 and CPUID; `wbinvd` and
`invd`; the `lods` fast path for long memory reads; logical and
guest-physical address types (linear addresses of a VMX guest are
translated through EPT, but guest-physical addresses can't be given
directly).
