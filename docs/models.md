# Target models

libredci addresses TAPs and registers by name. A platform description says
which JTAG chains exist, which TAPs sit on them, how each TAP is reached and
which registers it has. Descriptions are Go values built from the types in
`pkg/model`, one package per platform under `pkg/platforms`. `pkg/tapnet`
turns accesses into JTAG scans.

## Concepts

A **platform** has one or more physical **chains** (over DCI, the bridge
selects the chain; see [protocol.md](protocol.md#chains)). Each chain holds a
tree of **TAPs**. A TAP has an IR length, optionally an IDCODE and the
instruction that selects it, registers, required preconditions, and children.
A child is reached from its parent through one of two kinds of **link**.

### Spliced link

The parent has a select register (instruction `SelectIR`, `Width` bits).
Writing 1 to bit `Bit` and passing Update-DR inserts the child into the
physical chain **directly on the TDO side of the parent**. Writing all zeros
removes all children again.

A child can need siblings on the same select register inserted with it
(`With`): all their bits are set in one write, and the inserted TAPs sit in
select-bit order, the highest bit nearest TDO.

While spliced in, the child is a real member of the chain: every other TAP
must get an all-ones IR (BYPASS, one DR bit), and the bits shifted first (LSB)
belong to the TAP nearest TDO. Example, reading the IDCODE of
`pch.left.csme_par.csme`, three levels deep with 8-bit IRs:

```
IR[8]  = 11            DR[16] = 0x0004   pch: splice in pch.left (bit 2)
IR[16] = ff 11         DR[15] = 0x0004   pch.left: splice in csme_par (bit 2 of 14), + 1 bypass bit
IR[24] = ff ff 11      DR[16] = 0x0004   csme_par: splice in csme (bit 2 of 14), + 2 bypass bits
IR[32] = ff ff ff 0c   DR[35]            csme: IDCODE, + 3 bypass bits
IR[32] = ff ff 11 ff   DR[17] = 0        remove, deepest first
IR[24] = ff 11 ff      DR[16] = 0
IR[16] = 11 ff         DR[17] = 0
```

### Virtual link

The child never appears on the physical chain. The parent has two
instructions:

* `IRSelect`: the parent's DR is the child's IR, with `IRPadLo`/`IRPadHi`
  pad bits around it.
* `DRSelect`: the parent's DR is the child's DR, with `DRPadLo` pad bits on the
  low side (shifted out first) and `DRPadHi` on the high side.

Example, reading CBO0's IDCODE behind the 9-bit KBL uncore TAP:

```
IR[9] = 0x100   DR[8]  = 0x02     child IR := IDCODE
IR[9] = 0x101   DR[40] = 0        8 pad bits + 32-bit IDCODE
```

Links compose: virtual children of spliced TAPs and virtual children of
virtual TAPs both work. Every access, including the splicing around it, runs
as one JTAG transaction that starts and ends in Run-Test/Idle.

## Registers

A register is an IR opcode, a length in bits, named fields and an access kind:

* **read-only**: read by scanning zeros through it.
* **read-write**: whatever is shifted in is latched at Update-DR, so reading
  also writes. Such registers are read with an *exchange* scan: one DR scan
  of 2×n bits with a pause between the halves. The first half captures the old
  contents, and the second half is what remains in the register. Some
  read-write registers are conventionally read by scanning zeros through them
  (their writable fields are normally 0); the model marks these.
* **write-only**.

## Preconditions

Some accesses need target state set up first, for example TAP arbitration or
keeping a power domain awake. TAPs and registers name the preconditions they
need. A session acquires them in dependency order on first use and releases
them in reverse order at the end, restoring everything they changed.

## Platform `kbl-spt`: Kaby Lake U/R + Sunrise Point-LP

### Chain 0: PCH

Root `pch`: 8-bit IR, IDCODE `0x?a506013` (IR `0x02`). All children have 8-bit
IRs and IDCODE instruction `0x0c`, and are spliced through select instruction
`0x11`.

| TAP | Parent | Select bit / width | IDCODE |
|---|---|---|---|
| `pch.master` | `pch` | 0 / 16 | `0x02080001` |
| `pch.master.tpsb` | `pch.master` | 2 / 24 | `0x00082003` (see below) |
| `pch.master.npk` | `pch.master` | 6 / 24 | `0x00082007` (see below) |
| `pch.master.retime` | `pch.master` | 22 / 24 | `0x00082017` |
| `pch.left` | `pch` | 2 / 16 | `0x02080003` |
| `pch.left.csme_par` | `pch.left` | 2 / 14 | `0x02086103` |
| `pch.left.csme_par.csme` | `pch.left.csme_par` | 2 / 14 | `0x08086101`: CSME core TAP |
| `pch.left.csme_par.retime` | `pch.left.csme_par` | 12 / 14 | `0x0008610b` |
| `pch.lb` | `pch` | 4 / 16 | `0x02080005` |
| `pch.lb.ish_par` | `pch.lb` | 0 / 14 | `0x02088201` |
| `pch.lb.ish_par.ish` | `pch.lb.ish_par` | 0 / 12, with `retime` | `0x28289013` (IR `0x02`): ISH core TAP |
| `pch.lb.ish_par.retime` | `pch.lb.ish_par` | 10 / 12 | `0x0008800b` |
| `pch.bottom` | `pch` | 6 / 16 | `0x02080007` |
| `pch.top` | `pch` | 8 / 16 | `0x02080009` |
| `pch.agg` | `pch` | 10 / 16 | `0x0008000b` |
| `pch.cltap_retime` | `pch` | 14 / 16 | `0x0008000f` |

`pch.lb.ish_par.ish` is spliced in together with `pch.lb.ish_par.retime`
(select `0x401`); on its own it does not pass data. IR `0x0c` reads
`0x08088001`. With the ISH disabled (see the README's target setup) it
holds TDO low and reads 0.

On the T480 (SPT-LP C1), splicing in `pch.master.tpsb` or `pch.master.npk`
holds TDO low for the whole chain: the TAP is inserted but does not operate,
so its IDCODE reads 0. Splice them out again before other accesses.

**`pch_tdo_hold_released`**: required before using instructions `0x11` and
`0xc1` on `pch`. Release with IR `0xff` and a 2-bit DR of `0`; restore with `0b11`.

### Chain 1: CPU

Root `cpu` (uncore): **9-bit IR**, IDCODE `0xaa76d013` (IR `0x02`). Its children
are virtual, with no IR padding and `DRPadLo` = 8.

| TAP | IR / DR select | IR | Requires |
|---|---|---|---|
| `cpu.cbo0`–`3` | `0x100+4n` / `0x101+4n` | 8; IDCODE `0x0a76b013` (IR `0x02`) | arbitration, CBOs awake |
| `cpu.core0`–`3` | `0x110+4n` / `0x111+4n` | 8 | arbitration, cores awake |

Link-status registers on `cpu` tell which units exist: `cbo0`–`3_link`
(IR `0x103+4n`) and `core0`–`3_link` (IR `0x113+4n`). They are 31 bits wide,
with bit 8 = exists and bit 29 = power good, and read `0x20000100` for a
present unit. They are read by scanning zeros. The core links only respond
while the cores are awake.

Uncore registers used by the preconditions:

| Register | IR | Bits | Fields |
|---|---|---|---|
| `tap_handshake` | `0x7f` | 7 | 0 probe owns, 1 probe request, 2 VCU owns, 4 VCU request |
| `pcu_bclk_ctrl` | `0x50` | 18 | 1 C6/C7 wake, 2 C6/C7 defer, 3 ignore wake, 13:8 boot FSM state (`0x20` = system agent up) |
| `pcode_wake_req` | `0x4f` | 65 | 8:1 core wake-up, 35:33 core C-state limit, 40:37 package C-state limit, 50:43 core PREQ enable, 53 turbo disable, 54 freeze IA frequency, 55 probe-mode pcode flow, 56 SAGV disable |

Preconditions:

* **`cpu_tap_arbitrated`**: exchange `tap_handshake` with "probe request".
  The register then reads `0b11`. Release by writing "probe owns".
* **`cpu_package_awake`**: write `pcu_bclk_ctrl = 0x6` repeatedly until the
  captured value shows wake = 1, defer = 1, ignore = 0 and boot FSM state
  `0x20` (or `0x11`), then wait 1 ms. Release by writing 0.
* **`cpu_cores_awake`** (needs package awake): set core wake-up for all
  present cores, core C-state limit 1 and package C-state limit 0 in
  `pcode_wake_req`, then wait 50 ms. Release by restoring the previous value.
  The wake-up bits read back as 0 (self-clearing).
* **`cpu_cbos_awake`** (needs package awake): as above, plus turbo disable,
  freeze IA frequency and SAGV disable, then wait 5 ms.

Core TAP registers: `tap_status` (IR `0x60`, 37 bits, read-only; `0x0000400002`
when idle), `probe_mode` (`0x86`, 4 bits), `pdr_t0`/`pdr_t1` (`0x84`/`0x85`,
66 bits), `sub_pir_write` (`0x83`, 134 bits).

### Uncore CSR and MMIO access through the VCU

The uncore's validation control unit (VCU) executes commands posted to a
mailbox on the `cpu` TAP. It only needs `cpu_tap_arbitrated`; no core has to
be halted.

* `vcu_mailbox` (IR `0xc9`, 65 bits): bit 1 run/busy, 16:9 index, 32:17
  opcode, 64:33 data. Written with an exchange scan.
* `vcu_mailbox_read` (IR `0xcb`, same layout): read by scanning zeros. Once
  run/busy is clear, the opcode field holds the completion code (`0x40` success,
  `0x80` timeout, `0x82` thread unavailable, `0x90` illegal) and the data field
  holds the result.
* Commands: `0x02` open sequence, `0x03` close sequence, `0x06` read data,
  `0x07` write data, `0x11`/`0x12` read/write local CSR, `0x13`/`0x14`
  read/write MMIO. Sequence IDs: 1 read CSR, 2 write CSR, 3 read MMIO,
  4 write MMIO.
* A read is `open(seq)`, `read-op(address)`, `read data` (twice for 8 bytes),
  `close(1)`. A write is `open(seq)`, `write-op(address)`, `write data`
  (twice for 8 bytes), `close(1)`.
* Address word: offset [31:16], size code [15:14] (0–3 = 1/2/4/8 bytes),
  device [13:11], function [10:8], BAR [3:2] (MMIO only).

"Local CSR" is the system agent's own PCI configuration space; the device
field is ignored. Examples on KBL-R U: offset `0x0` = `0x59148086`, `0x48`
(MCHBAR) = `0xfed10001`, `0x60` (PCIEXBAR) = `0xe0000001`. MMIO reaches
system-agent devices only, and decodes the device number partially (device
`0x16` aliases device 2, the iGPU). PCH devices are refused with "illegal".
