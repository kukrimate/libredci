# Kaby Lake U/Y/R CPU

`pkg/cpu/kbl` describes chain 1 of a Kaby Lake U/Y (incl. KBL-R) CPU. See
[model.md](../model.md) for links, registers and preconditions, and
[runctl/bigcore.md](../runctl/bigcore.md) for run control of its cores.

## TAPs

Root `cpu` (uncore): **9-bit IR**, IDCODE `0xaa76d013` (IR `0x02`). Its children
are virtual, with no IR padding and `DRPadLo` = 8.

| TAP | IR / DR select | IR | Requires |
|---|---|---|---|
| `cpu.cbo0`–`3` | `0x100+4n` / `0x101+4n` | 8; IDCODE `0x0a76b013` (IR `0x02`) | arbitration, CBOs awake |
| `cpu.core0`–`3` | `0x110+4n` / `0x111+4n` | 8 | arbitration, cores awake |

Link-status registers on `cpu` tell which units exist: `cbo0`–`3_link`
(IR `0x103+4n`) and `core0`–`3_link` (IR `0x113+4n`). They are 31 bits wide,
with bit 8 = exists, 27 FSM error, 28 access without power, 29 power good
and 30 disabled, and read `0x20000100` for a present unit. They are read by
scanning zeros, the CBO links with the CBOs awake and the core links with
the cores awake. After the wake request they read 0 (and briefly other
values, e.g. `0x7ffffffe`) for up to ~650 ms when the package was idle, so
a read counts once power good = 1, FSM error = 0 and access without power =
0; the link is re-read for up to 2 s until then. A unit found present stays
present for the session; an absent result is read again on the next
access.

Core TAP registers: `tap_status` (IR `0x60`, 37 bits, read-only; `0x0000400002`
when idle), `probe_mode` (`0x86`, 4 bits), `pdr_t0`/`pdr_t1` (`0x84`/`0x85`,
66 bits), `sub_pir_write` (`0x83`, 134 bits).

## Preconditions

Uncore registers used by the preconditions:

| Register | IR | Bits | Fields |
|---|---|---|---|
| `tap_handshake` | `0x7f` | 7 | 0 probe owns, 1 probe request, 2 VCU owns, 4 VCU request |
| `pcu_bclk_ctrl` | `0x50` | 18 | 1 C6/C7 wake, 2 C6/C7 defer, 3 ignore wake, 13:8 boot FSM state (`0x20` = system agent up) |
| `pcode_wake_req` | `0x4f` | 65 | 8:1 core wake-up, 35:33 core C-state limit, 40:37 package C-state limit, 50:43 core PREQ enable, 53 turbo disable, 54 freeze IA frequency, 55 probe-mode pcode flow, 56 SAGV disable |

* **`cpu_tap_arbitrated`**: exchange `tap_handshake` with "probe request".
  The register then reads `0b11`. Release by writing "probe owns". It is the
  `cpu` TAP's arbitration condition.
* **`cpu_package_awake`**: write `pcu_bclk_ctrl = 0x6` repeatedly until the
  captured value shows wake = 1, defer = 1, ignore = 0 and boot FSM state
  `0x20` (or `0x11`), then wait 1 ms. Release by writing 0.
* **`cpu_cores_awake`** (needs package awake): set core wake-up for all
  present cores, core C-state limit 1 and package C-state limit 0 in
  `pcode_wake_req`, then wait 50 ms. Release by restoring the previous
  values of these fields; the other fields (probe-mode flow, PREQ enables)
  are left as they are. The wake-up bits read back as 0 (self-clearing).
* **`cpu_cbos_awake`** (needs package awake): as above, plus turbo disable,
  freeze IA frequency and SAGV disable, then wait 5 ms.

All four are transient.

## CSR and MMIO access

`kbl.VCU` is the uncore's VCU ([ip/vcu.md](../ip/vcu.md)) on the `cpu` TAP.
It needs `cpu_tap_arbitrated` and `cpu_cores_awake`; no core has to be
halted. With only the package awake, commands complete with "timeout"; with
the package asleep, the mailbox reads 0.

"Local CSR" is the system agent's own PCI configuration space; the device
field is ignored. Examples on KBL-R U: offset `0x0` = `0x59148086`, `0x48`
(MCHBAR) = `0xfed10001`, `0x60` (PCIEXBAR) = `0xe0000001`.

MMIO on the T480 (KBL-R U): BAR 0 is MCHBAR (offset `0x0` reads the same as
physical `0xfed10000`, `0x5da8` as `0xfed15da8`). Devices 0, 2, 4 and 6
all give the same results, so device bits 2:1 are ignored; odd devices, a
non-zero function and BAR 2 are refused with "illegal". BAR 1 offset 0
reads `0x10`; what BAR 1 maps is unknown.
