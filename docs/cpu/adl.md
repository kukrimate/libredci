# Alder Lake-S CPU

`pkg/cpu/adl` describes chain 1 of an Alder Lake-S CPU: its root TAP only.

Root `cpu`: **12-bit IR**, IDCODE `0x?00f6113` (IR `0x02`). On the desktop
platform the PCH reaches the CPU's TAP over board traces. On the H610M-K
chain 1 reads 0 and `cltdbg` reports the CPU cluster TAP unavailable, also
with VCCSTG forced on.
