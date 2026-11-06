# Sunrise Point-LP PCH

`pkg/pch/spt` describes chain 0 of a Sunrise Point-LP PCH. See
[model.md](../model.md) for links, registers and preconditions.

## TAPs

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

## Preconditions

**`pch_tdo_hold_released`**: required before using instructions `0x11` and
`0xc1` on `pch`. Release with IR `0xff` and a 2-bit DR of `0`; restore with `0b11`.
