# Target models

libredci addresses TAPs and registers by name. A chip description says
which TAPs sit on the chip's JTAG chain, how each TAP is reached and which
registers it has. Descriptions are Go values built from the types in
`pkg/model`. `pkg/tapnet` turns accesses into JTAG scans.

## Chips and platforms

Each chip has a package under `pkg/pch` or `pkg/cpu` that returns a
`model.Chip`: its chain's root TAP, the conditions its TAPs use, and the
conditions to release after each operation. `pkg/platform` lists the known
chips, identifies them by their root TAP's IDCODE (ignoring the version
field), and composes a PCH (chain 0) and a CPU (chain 1) into a platform
with the access paths they offer beyond their TAPs, such as CSR access
through a VCU.

| Chip | Package | Description |
|---|---|---|
| Sunrise Point-LP PCH | `pkg/pch/spt` | [pch/spt.md](pch/spt.md) |
| Kaby Lake U/Y/R CPU | `pkg/cpu/kbl` | [cpu/kbl.md](cpu/kbl.md) |

Chips instantiate building blocks from `pkg/ip`, which take TAP and
condition names as parameters:

| Block | Package | Description |
|---|---|---|
| VCU: uncore CSR and MMIO access | `pkg/ip/vcu` | [ip/vcu.md](ip/vcu.md) |

## Concepts

A **platform** has one or more physical **chains** (over DCI, the bridge
selects the chain; see [protocol.md](protocol.md#chains)). Each chain holds a
tree of **TAPs**. A TAP has an IR length, optionally an IDCODE and the
instruction that selects it, registers, required preconditions, and children.
A child is reached from its parent through one of two kinds of **link**.

### Spliced link

The parent has a select register (instruction `SelectIR`, `Width` bits).
Writing 1 to bit `Bit` and passing Update-DR inserts the child into the
physical chain **directly on the TDO side of the parent**, between the
parent and any TAPs spliced in earlier further towards TDO. Writing all
zeros removes all children again. Splicing persists through Test-Logic-Reset.

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
  contents, and the second half is what remains in the register. A
  read-modify-write (on a chain's root TAP) splits that scan over two
  transactions: the first shifts the new field values over zeros while
  capturing the old contents and stops in Pause-DR; the second
  shifts the old contents with the new field values. Only the second value
  is latched. Some read-write registers are conventionally read by scanning
  zeros through them (their writable fields are normally 0); the model
  marks these.
* **write-only**.

## Preconditions

Some accesses need target state set up first, for example TAP arbitration or
keeping a power domain awake. TAPs and registers name the preconditions they
need. A session acquires them in dependency order on first use and releases
them in reverse order at the end, restoring everything they changed.

A chip lists **transient** conditions, which the shell releases after each
command (on Kaby Lake the CPU wake requests and the uncore TAP
arbitration); the next access acquires them again. A TAP can name the
condition that **arbitrates** the TAP network below it; run control
releases it at the end of a halt or resume.
