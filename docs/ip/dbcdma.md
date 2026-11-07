# xHCI debug capability DMA

`pkg/ip/dbcdma` programs the xHCI debug capability's GP2 DMA engine through
the sideband. A PCH package exports its engine as `DMA`, and
`pkg/platform` offers it as the platform's `DMA`, which the shell's `dma`
command uses. The xHCI performs the memory access.

GP2 IN (memory to probe) registers, 32 bits each, at the engine's sideband
port, BAR 5:

| Offset | Register |
|---|---|
| `0x1c`, `0x20` | payload address low, high |
| `0x24` | payload qualifier: PSF destination of the payload address |
| `0x28` | length; bit 31 = valid, setting it starts the transfer |
| `0x2c`, `0x30` | status address low, high |
| `0x34` | status qualifier |

A PSF destination is decode mode [21:20], PSF [19:16], port group [15],
port [14:11]. A read first writes `0xdeadbeef` to payload low and fails
unless it reads back, then programs the registers in offset order with the
length last. The payload arrives on the DMA interface
([protocol.md](../protocol.md#usb-device)).

A PSF decode mode of 1 sends a write upstream to DMI, which reaches host
memory. Mode 2 sends it to the PSF port named by the PSF, port group and
port fields. The status needs a destination that accepts the write: a
wrong one can hang the platform (see
[Sunrise Point-LP](../pch/spt.md#dma)).

Chips with a DMA engine: [Alder Point-S](../pch/adp.md#dma). Sunrise Point-LP
has the engine but no status route without a halt yet
([pch/spt.md](../pch/spt.md#dma)).
