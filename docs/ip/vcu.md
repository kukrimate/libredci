# VCU

`pkg/ip/vcu` drives a CPU uncore's validation control unit (VCU), which
executes commands posted to a mailbox on the uncore TAP. Through it the
probe reads and writes uncore-local CSRs and device MMIO without halting a
core. A chip creates a `vcu.Unit` for its uncore TAP and adds the mailbox
registers with the conditions its commands need.

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

Chips with a VCU: [Kaby Lake](../cpu/kbl.md#csr-and-mmio-access).
