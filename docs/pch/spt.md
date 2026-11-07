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

`pch.master.tpsb` and `pch.master.npk` operate only with the Trace Hub
enabled by host firmware (FSP-M `EnableTraceHub`; in coreboot, `device ref
tracehub on end` in the devicetree). With it disabled, splicing either in
holds TDO low for the whole chain and its IDCODE reads 0; splice it out
again before other accesses. With it enabled, both read their IDCODEs. The
Trace Hub PCI function (`00:1f.7`) stays hidden from the host either way.

## Preconditions

**`pch_tdo_hold_released`**: required before using instructions `0x11` and
`0xc1` on `pch`. Release with IR `0xff` and a 2-bit DR of `0`; restore with `0b11`.

## Sideband

`spt.TPSB` is `pch.master.tpsb`, a generation 2.0 TAP2IOSFSB (see
[ip/iosfsb.md](../ip/iosfsb.md)). It needs the Trace Hub enabled (above).

On the T480 (with the Trace Hub enabled, Red Unlock):

* A CR read of GPIO community 0 (port `0xaf`) offset `0x400` returns the
  same value as the host's P2SB window, with and without the extended
  header.
* The xHCI is port `0xe6` (as on Lewisburg); a config read of FID `0xa0`
  offset 0 returns `0x9d2f8086`. Port `0x70`, the xHCI on Cannon Point and
  later, returns unsupported request.
* The DbC GP2 registers are at port
  `0xe6`, BAR 5, offsets `0x00`–`0x34`. They read 0 after boot, and a write
  to IN payload low (`0x1c`) reads back.
* The Trace Hub (NPK) is port `0xb6`, FID 0: a config read of offset 0
  returns `0x9d268086`. With the Trace Hub enabled, firmware sets its MTB
  BAR to `0xfe100000` and its SW BAR to `0xfe200000`. Memory reads and
  writes with BAR 0 reach the MTB registers, for example the DbC
  status-data register at `0xa100c`.
* PSF3 (port `0xbc`) holds the Trace Hub's shadow configuration at private
  offset `0x300` (BAR0 `0xfe100000`, BAR2 `0xfe200000`, `0x31c` = 2).

Every port number checked matches the sideband port list Goryachy and
Ermolov published for Lewisburg (Black Hat Asia 2019): xHCI `0xe6`, NPK
`0xb6`, GPIO community 0 `0xaf`, PSF3 `0xbc`.

Whether the T480's TPSB works without Red Unlock is untested.
