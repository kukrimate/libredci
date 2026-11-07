# IOSF sideband through a TAP2IOSFSB

`pkg/ip/iosfsb` sends IOSF sideband messages through a PCH's TAP2IOSFSB
(TPSB) TAP. A PCH package exports its bridge as `TPSB`; `pkg/platform`
offers it as the platform's `Sideband`, which the shell's `sb` command
uses.

| Register | IR | Bits | Use |
|---|---|---|---|
| `sbmsggo` | `0x30` | 96 | message: 3 non-posted, 4 extended data, 15:8 port, 23:16 opcode, 29:27 BAR, 30 extended address, 31 extended header, 35:32 first byte enables, 39:36 second byte enables, 47:40 FID, 63:48 address, 95:64 data; updating it sends the message |
| `sbmsgrsp` | `0x31` | 96 | 1:0 status (3 = complete), 28:27 response (0 OK, 1 unsupported request, 2 powered down), 95:32 read data |
| `sbextaddr` | `0x33` | 32 | address bits 47:16 |
| `sbrootspc` | `0x35` | 5 | root space, sent with the extended header |
| `sbextdatawr` | `0x36` | 32 | write data bits 63:32 |

An access writes `sbrootspc`, then `sbextaddr` for offsets above `0xffff`,
then `sbmsggo`. A read polls `sbmsgrsp` up to 6 times until the status is
complete.

A generation 2.0 bridge (Sunrise Point, Lewisburg, Cannon Point) has no
extended data or second byte enables; 64-bit accesses are split into two
32-bit ones.

Chips with a TPSB: [Sunrise Point-LP](../pch/spt.md#sideband),
[Alder Point-S](../pch/adp.md#sideband).
