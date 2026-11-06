# Run control

`pkg/runctl` provides architecture-independent run control: a `Thread` can be
halted and resumed, and its registers, physical memory and I/O ports
accessed. Optional interfaces add MSRs, hardware breakpoints and
single-stepping. Platform models declare the threads on each TAP; an
implementation is selected by the thread's architecture.

| Architecture | Threads | Package |
|---|---|---|
| `lakemont` | CSME and ISH cores of SPT (`csme` on `pch.left.csme_par.csme`, `ish` on `pch.lb.ish_par.ish`) | `pkg/runctl/lakemont` |
| `kbl-core` | Kaby Lake cores (`cpu.c<core>t<thread>`) | `pkg/runctl/bigcore` |

The backends are described in [lakemont.md](lakemont.md) (CSME and ISH)
and [bigcore.md](bigcore.md) (host cores).
