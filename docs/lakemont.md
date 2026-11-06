# Lakemont run control

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

## Probe instructions

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

## Memory, I/O and MSRs

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

## Halt

1. Write probe-mode request = 1. Poll status until bit 2 is set and bit 1 is clear.
2. Save EFLAGS, CS base/limit/AR, SS AR, DS base/limit/AR, CR0 and DR7. The
   CS and SS AR values are kept with AR bits 13:8 (register bits 21:16:
   limit 19:16, AVL, L) clear, as OpenIPC does.
3. Set up a flat environment in OpenIPC's order (`save_minimum_state`): DS
   base 0, limit `0xffffffff`, AR `0x004f9300`; DR7 = 0; CS the same as DS;
   EFLAGS.VM cleared if set; SS AR `0x004f9300` if SS.DPL isn't 0; EFLAGS.IF
   cleared if set; CR0.PG cleared if set; CR0.CD and NW set if CD is clear.
   OpenIPC then issues WBINVD when CS.DPL is 0; libredci doesn't. On the
   SPT CSME a WBINVD probe instruction was observed to leave the core busy
   (an observation from the original implementation, not re-tested; OpenIPC
   only issues it for a ring-0 stop, and the CSME stopped in ring 3 in all
   later tests).
4. DR6 gives the stop reason. EIP (plus CS base) gives the location.

Save EAX, ECX and EDX before using them for memory, I/O or MSR accesses.

## Resume

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

## Breakpoints and single-step

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
program's own DR7 and PMCR once no breakpoints remain. A slot's program DRn
is saved when a breakpoint first uses it (also in the state file) and is
written back once the slot is cleared; `reg drN` shows the program's value
meanwhile. (OpenIPC writes 0 to a cleared slot.)

The Lakemont register opcodes, the memory/I/O probe instructions (`mov
dl/dx/edx, [eax]`, `mov [eax], dl/dx/edx`, `in`/`out` with DX), MSR access
(refused unless CS.RPL is 0) and stepping (EFLAGS.TF and RF, PMCR.IR) are
the same as OpenIPC's (`common/ia/lakemont`, `lmt/tap.py`). Differences:
libredci only accesses MSRs listed as known to exist unless forced, keeps
RFLAGS.TF unless its own step set it (OpenIPC clears it at every halt),
keeps the halted state in a state file across sessions, and lacks software
breakpoints, branch stepping, the shutdown and machine-check break
conditions, MMX/SSE registers and the 128-bit PDR.

## Reset break

Writing 1 to IR `0x35` makes the core stop in probe mode at its reset vector
on its next reset; status bit 18 is then set. The register doesn't read back
the written value. The state at a reset-break stop is the reset state, so it
can be resumed without restoring anything.

On SPT the CSME resets only with the platform, and the rest of the platform
reset waits for it. With the break armed on the CSME (`resetbreak on`), the
DCI platform reset (`reset`) stops the CSME at its reset vector (CS
`f000`, EIP `fff0`, CS base `ffff0000`) before the rest of the platform is
reset: the host freezes and DCI stays up. `attach` adopts the stopped CSME,
whose registers and memory can then be read. When the CSME resumes, the
platform reset completes and the platform boots, with the CSME firmware
running normally. Disarm the break (`resetbreak off`) before resuming:
with the break still armed, the CSME was found stopped again and the host
didn't boot.
