# DCI DbC wire protocol

This is the protocol between a debug host and the **ExI bridge** of an Intel
PCH when Direct Connect Interface (DCI) runs in **DbC mode**. In that mode the
target's xHCI Debug Capability shows up on the host as a USB device, and all
debug traffic (bridge control and JTAG) goes over one pair of bulk endpoints.

Applies to: Sunrise Point-LP (ThinkPad T480, USB ID `8087:0a6e`). Values marked
"required" must be written as given; their individual function is not documented.

## USB device

| Field | Value |
|---|---|
| Vendor | `0x8087` |
| Product | `0x0a6e` on SPT-LP. Intel uses `0x0a6e`–`0x0a79` and `0x0ae6`–`0x0aef` for DCI DbC devices. |
| Speed | SuperSpeed |
| Configuration | 1, four interfaces of class `0xDC` (diagnostic), grouped by one IAD |

| Interface | Endpoints | Use |
|---|---|---|
| 0 | none | — |
| 1 | bulk OUT `0x03`, bulk IN `0x83`, 1024 bytes | **ExI packets: everything in this document** |
| 2 | bulk IN `0x84` | trace |
| 3 | bulk OUT `0x02`, bulk IN `0x82` | DMA |

Only interface 1 is needed for debugging.

## Packets

All traffic on endpoints `0x03`/`0x83` consists of **64-byte packets**. A bulk
transfer carries 1–16 of them back to back (at most 1024 bytes); read with a
1024-byte buffer. Byte 0 is the packet type. Multi-byte fields are little-endian.

| Type | Host → target | Target → host |
|---|---|---|
| `0x01` | config: register writes | status |
| `0x03` | JTAG: TCK commands | TDO data |

## Config packets (type `0x01`, host → target)

```
byte 0     0x01
byte 1     sequence number (starts at 1 each session, +1 per packet)
byte 2     0x01 = reply with a status packet, 0x00 = no reply
byte 3     0x00
byte 4..   up to 15 register writes of 4 bytes, zero padded:
             [register u8][bank u8][value u16]
```

A status reply to a config packet carries the low 7 bits of that packet's
sequence number in byte 3.

### Registers

Bank 0 belongs to the bridge, bank 1 to the JTAG engine.

| Register | Function |
|---|---|
| 0.`0x01` | **Arbitration.** 2 = request debug ownership, 0 = release. |
| 0.`0x06`–`0x09` | **Unlock key**: `0x49d6 0xa232 0x2ddf 0x2b99`. Cleared (written 0) on disconnect. |
| 0.`0x0a` | Required: `0x3c00` on connect and on disconnect. |
| 0.`0x0b` | Session value: any value on connect, 0 on disconnect. The status packet echoes its low byte. |
| 1.`0x00` | **JTAG control.** Bit 1 selects the chain (see [Chains](#chains)). Bit 5 enables trigger waits (together with 1.`0x02` = `0x00a8`, see below). Bit 15 requests a pin snapshot: the next status packet carries pin state, and the bit clears itself. |
| 1.`0x01` | Per-chain setting; see [Chains](#chains). |
| 1.`0x02` | `0x0098` normally, `0x00a8` with trigger waits enabled. |
| 1.`0x03`, 1.`0x06` | Required values; see [Session](#session). |
| 1.`0x05` | **Engine and pin outputs.** Bit 0 must be set for the engine to run. **Bit 1 = PREQ** to the CPU. **Bit 2 = platform reset**: hold for ~170 ms; the target resets and the DbC link drops. On some hosts the DbC device only re-enumerates after the USB cable is re-plugged. Bit 11 = boot-stall hook 3, bit 15 = early-break hook 2. |

## Status packets (type `0x01`, target → host)

```
byte 0     0x01
byte 1     target transmit counter (free running)
byte 2     sequence number of the last JTAG packet processed
byte 3     low 7 bits of the sequence number of the last config packet processed
byte 4..   16-bit words; word i is at byte 4 + 2i
```

| Word | Content |
|---|---|
| 0, 1 | Both `2` while the host owns the bridge, `0` otherwise. |
| 8 | In a pin snapshot: bit 1 = PREQ asserted. |
| 14 | High byte: ExI SIP version (`0x28` = generation 2, PCH client, on SPT-LP). Low byte: low byte of 0.`0x0b`. |
| 16–25 | Bank-1 registers 0–9, echoed. |

Status packets also follow every completed JTAG transaction.

## JTAG packets (type `0x03`, host → target)

```
byte 0     0x03
byte 1     JTAG sequence number (8-bit, wraps)
byte 2     0x00 = more packets of this scan follow
           0x03 = last packet: execute and return TDO
byte 3     0x80, or 0x81 if the packet contains wait commands
byte 4..5  0
byte 6..7  payload length in bits = 4 × number of TCK commands
byte 8..   TCK commands, 4 bits each, low nibble first (max. 112 per packet)
```

Each TCK command is one clock:

| Bit | Meaning |
|---|---|
| 0 | TMS for this clock |
| 1 | TDI for this clock |
| 2 | capture TDO on this clock |
| 3 | wait: hold this clock until the trigger input fires |

The host drives the TAP state machine directly. Reading an IDCODE through an
8-bit IR (nibbles in hex, `2x4` = four times `2`):

```
2x4          Run-Test/Idle, TDI high
3 3 2 2      Select-DR, Select-IR, Capture-IR, Shift-IR
4 6 4 4 4 4 4 5   IR = 0x02, LSB first; TMS on the last bit (Exit1-IR)
3 2          Update-IR, Run-Test/Idle
3 2 2        Select-DR, Capture-DR, Shift-DR
4x31 5       32 bits captured; TMS on the last bit (Exit1-DR)
3 2          Update-DR, Run-Test/Idle
```

**Trigger waits.** With trigger waits enabled (bank-1 reg 0 bit 5,
reg 2 = `0x00a8`), a command with bit 3 set stalls the engine until the
trigger input fires. On the CPU chain the trigger is the cores' probe-mode
ready signal: after submitting a probe-mode instruction (Update-DR, one
Run-Test/Idle clock), a wait command in Run-Test/Idle holds until the
instruction has completed. A core signals probe-mode ready only while all
of its threads are in probe mode.

A scan longer than 112 clocks is split across consecutive packets. All but the
last have byte 2 = `0x00`. Any number of packets may be sent back to back.

**The JTAG sequence number is kept by the target across sessions.** After
arbitration, continue from the value in byte 2 of the first status packet,
plus 1.

## TDO packets (type `0x03`, target → host)

```
byte 0     0x03
byte 1     target transmit counter
byte 2     sequence number of the host packet this belongs to
byte 3     0x80 | (last config sequence & 0x7f)
byte 6..7  number of TDO bits in this packet (max. 448)
byte 8..   TDO bits, LSB first, in capture order
```

Captured bits come back in as many TDO packets as needed. The transaction is
complete once a packet echoing the **last** host packet's sequence number
has arrived **and** all expected bits have been received: more than one packet
can echo the last sequence number. A transaction without captures returns
one TDO packet with 0 bits.

## Session

Each step waits for its status reply unless noted.

```
connect
  b0.r1 = 2                         check status words 0,1 == 2;
                                    JTAG sequence := status byte 2
  b0.r6..r9 = 49d6 a232 2ddf 2b99   unlock
  b0.ra = 3c00, b0.rb = <any>
  b1.r6 = ff0e
  b1.r2 = 0
  b1.r0 = 1a11                      twice
  b1.r0 = 1811, b1.r1 = 0028, b1.r2 = 0098, b1.r3 = 00f0, b1.r5 = 1
  b1.r5 = 0, then b1.r5 = 1         no status

select chain n
  b1.r0, b1.r1 = values from the chain table
  b1.r5 = 0, then b1.r5 = 1         no status

disconnect
  b0.rb = 0, b0.ra = 3c00, b1.r0..r3 = 0, b1.r5 = 0, b1.r6 = 0,
    bytes 00 30 00 00, b0.r12 = 0, b0.r11 = 0
  b0.r6..r9 = 0
  b0.r1 = 0                         release arbitration
```

## Chains

| Chain | b1.r0 | b1.r1 | Root TAP (KBL-U + SPT-LP) |
|---|---|---|---|
| 0 | `0x1811` | `0x0028` | PCH: 8-bit IR, IDCODE (IR `0x02`) `0x?a506013` |
| 1 | `0x1813` | `0x0020` | CPU uncore: **9-bit IR**, IDCODE (IR `0x02`) `0xaa76d013` |

The TAPs behind these roots are described in [models.md](models.md).

> **Warning:** shifting the wrong number of IR bits selects an unrelated
> instruction. A following DR scan can then write arbitrary data into target
> registers, and on the T480 that has powered the platform off. Use the IR
> lengths from the platform model.
