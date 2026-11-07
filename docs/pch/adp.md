# Alder Point-S PCH

`pkg/pch/adp` describes chain 0 of an Alder Point-S PCH. See
[model.md](../model.md) for links, registers and preconditions.

## TAPs

Root `pch`: 8-bit IR, IDCODE `0x?00e6113` (IR `0x02`). The table of TAPs
below it is in `pkg/pch/adp/taps.go`; `dcitool -pch adp -list` prints
the tree. The levels:

| Level | TAPs | Select |
|---|---|---|
| `pch.cl` | cluster | root, IR `0x12`, bit 0 of 4 |
| `pch.cl.bottom`, `.left`, `.right`, `.top` | regions | `0x11`, bits 0–6 of 40 |
| `pch.cl.<region>.<x>_par`, `.master`, `.master_vnn` | partitions | `0x11`, even bits of 40 |
| below the partitions | IP TAPs | `0x11`, even bits of 24 (22 in some partitions) |

All TAPs have 8-bit IRs and IDCODE instruction `0x0c`, except some IP TAPs
(IR lengths 5, 7 and 9; the PHY TAPs use IDCODE instruction `0x01`).

Each parent with children also has a 2-bit-IR **retime** TAP, `<parent>.retime`
(BYPASS = `0b11`), at the highest select bit (bit 2 of 4 on the root, 38 of
40, 22 of 24). Every child is spliced in together with its parent's retime,
which then sits between the child and the retimes of the levels above.
Reading the IDCODE of `pch.cl.bottom`, chain order TDI to TDO `pch`,
`pch.cl`, `pch.cl.bottom`, `pch.cl.retime`, `pch.retime`:

```
IR[8]  = 12                DR[4]  = 0b0101          pch: splice in pch.cl + pch.retime
IR[18] = ff 11 3           DR[42] = bits 38, 0, + 1 pch.cl: splice in bottom + pch.cl.retime
IR[28] = ff ff 0c 3 3      DR[36]                   pch.cl.bottom: IDCODE
IR[28] = ff 11 ff 3 3      DR[44] = 0               remove, deepest first
IR[18] = 12 ff 3           DR[6]  = 0
```

On the H610M-K (no unlock), every region and partition reads its IDCODE.
Of the IP TAPs below the partitions, only `pch.cl.bottom.npk_par.npk`,
`pch.cl.left.master_vnn.tpsb` and `pch.cl.left.master.dfx_secagg` do; the
others read 0 and are marked optional.

Root registers:

| Register | IR | Fields |
|---|---|---|
| `cltcr1` | `0xc1` | 0 pmode force, 1 pmode TPE, 9:2 pmode state, 10 pmode enable |
| `cltdbg` | `0xc4` | 0 force VCCSTG on, 1 VCCST power good, 2/3 CPU C10 gate (PMC/DFX), 4 CPU cluster TAP available |
| `tapcr` | `0xcd` | straps and status, e.g. 0 boot halt strap, 25 SPI config done, 29 CSME reset |
