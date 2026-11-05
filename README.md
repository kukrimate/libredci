# libredci

Free, open-source host software for debugging Intel platforms through
**Direct Connect Interface (DCI)** over USB, in DbC mode. Written in Go; uses
libusb through [gousb](https://github.com/google/gousb) and runs wherever
those do.

## Features

* DCI DbC transport: bridge arbitration and unlock, JTAG chain selection,
  JTAG scans of any length, PREQ and pin snapshots, platform reset.
* Named access to TAPs and their registers through platform models,
  including TAP splicing, virtual chains and target preconditions.
* Uncore CSR and MMIO access through the VCU, without halting any core.
* Run control for Lakemont cores (CSME, ISH): halt and resume, registers,
  physical and linear memory, I/O ports, MSRs, hardware breakpoints and
  single-stepping.
* Run control for Kaby Lake cores: halt and resume, registers, physical
  and linear memory, I/O ports, MSRs, hardware breakpoints and
  single-stepping.
* `dcitool`: a command-line tool with an interactive debug shell.

## Supported targets

| Platform | Package | Example | DbC USB ID |
|---|---|---|---|
| Kaby Lake U/R + Sunrise Point-LP | `pkg/platforms/kblspt` | ThinkPad T480 | `8087:0a6e` |

## Target setup

### Cable

DCI DbC needs a USB 3 A-to-A cable that carries only the SuperSpeed lines.
On one connector, VBUS (pin 1), D− (pin 2) and D+ (pin 3) must be
disconnected; GND and the SuperSpeed pins stay. Either pull those three
pins out of a generic A-to-A cable's connector with tweezers, or use
DataPro's [USB 3.0 A-A debugging cable](https://www.datapro.net/products/usb-3-0-super-speed-a-a-debugging-cable.html).

Connect the target's DCI-capable USB 3 port to a USB 3 port on the host.

### Enabling DCI

DCI can be enabled in two ways:

* **Flash descriptor.** A bit in the descriptor's PCH straps. The ME
  firmware reads it and enables DCI before the host CPU leaves reset; it
  only honours the bit when the file `/home/mca/eom` in the ME's MFS file
  system contains `0x00`. The coreboot change
  [util/ifdtool: Support the DCI enable bit on Skylake/Kaby Lake](https://review.coreboot.org/c/coreboot/+/82272)
  adds support for setting the bit to `ifdtool`.
* **Host firmware.** An IOSF sideband write: bit 4 of byte 4 on port
  `0xb8`. DCI is then available from that point in the boot onwards.

With DCI enabled, the target shows up on the host as the DbC USB device
listed under [Supported targets](#supported-targets).

### Host core run control

Halting and debugging the host CPU cores needs firmware consent: bit 0
(`DEBUG_ENABLE`) of `IA32_DEBUG_INTERFACE` (MSR `0xc80`) must be set
before bit 30 (`DEBUG_LOCK`) locks the MSR. In coreboot on the ThinkPad
T480 this can be done at the start of `mainboard_memory_init_params()`,
before FSP-M runs:

```c
msr_t msr = rdmsr(IA32_DEBUG_INTERFACE);	/* 0xc80 */
if (!(msr.lo & DEBUG_LOCK)) {			/* bit 30 */
	msr.lo |= DEBUG_ENABLE;			/* bit 0 */
	wrmsr(IA32_DEBUG_INTERFACE, msr);
}
```

### CSME run control

Debugging the CSME (Lakemont) core needs Red Unlock. On Skylake and Kaby
Lake, [deguard](https://doc.coreboot.org/soc/intel/deguard.html) enables
it.

### ISH run control

The Integrated Sensor Hub (ISH) core is a Lakemont core. Debugging it needs
Orange (OEM) Unlock, which Red Unlock includes, and the ISH enabled:

* **PCH strap:** `PchStrapIshDisable` clear, bit 7 at byte `0xc0` of the
  flash descriptor's PCH strap table on Sunrise Point-LP (`0x118` on
  SPT-H).
* **Host firmware:** the ISH PCI device (`00:13.0`) left enabled. In
  coreboot: `device ref ish on end` in the devicetree, which sets FSP's
  `PchIshEnable`.

Boards that use the ISH ship with both.

## Building

Needs Go 1.22 or later and the libusb-1.0 development files (Debian:
`libusb-1.0-0-dev`).

```
go build ./cmd/dcitool
```

On Linux, give your user access to the DbC device, e.g. with this udev rule:

```
SUBSYSTEM=="usb", ATTR{idVendor}=="8087", ATTR{idProduct}=="0a6e", MODE="0666"
```

Put any comment on its own line: current udev ignores rule lines with a
trailing `#` comment.

## Using dcitool

### TAP access

```
$ dcitool -list                          # TAPs and registers of the platform
$ dcitool -idcodes                       # read every TAP's IDCODE and compare to the model
pch                          0x9a506013  ok
  pch.master                   0x02080001  ok
  ...
cpu                          0xaa76d013  ok
  cpu.cbo0                     0x0a76b013  ok
$ dcitool -tap pch.left.csme_par.csme -ir 0xc -dr 32:0
pch.left.csme_par.csme ir 0xc dr 32:0x0 -> 0x08086101
$ dcitool -tap cpu.core0 -read tap_status
cpu.core0.tap_status = 0x0000400002
```

`-ir`, `-dr` and `-read` can be repeated and run in order. `-v` logs every
transaction, and `-vv` also logs raw USB packets.

Raw mode scans a physical chain directly, starting from Test-Logic-Reset:

```
$ dcitool -chain 0 -ir 8:0x2 -dr 32:0
ir 8:0x2 -> 0x01
dr 32:0x0 -> 0x9a506013
```

Raw mode doesn't check anything. Shifting the wrong number of IR bits selects an
unrelated instruction, and a following DR scan can write into arbitrary
target registers. Model mode uses the IR lengths from the platform description.

### Debug shell

`dcitool -shell` starts an interactive shell. `dcitool -c "cmd; cmd; …"`
runs commands and exits.

```
$ dcitool -c "halt; regs; md 0x1000 16; go"
csme halted at 0x9e17c (halt request)
eax       0435304c   ebx       00084348   ecx       00000000   edx       00000000
...
00001000  61 10 00 00 61 00 00 00  67 40 00 00 63 b0 00 00  |a...a...g@..c...|
csme running

$ dcitool -c "halt; bp 0 0x208a0; cont 5000; reg cs; rdmsr 0x10; bpc 0; go"
csme halted at 0x9e17c (halt request)
csme stopped at 0x208a0 (breakpoint 0)
cs = 0x8
msr 0x10 = 0x000000001caf2d46a0
csme running

$ dcitool -c "thread cpu.c0t0; halt; bp 0 0xfb918; cont 2000; trace 2; bpc 0; go"
cpu.c0t0 halted at 0xfb916 (halt request)
cpu.c0t0 stopped at 0xfb918 (breakpoint 0)
    0 000f8542  mov esi, eax
    1 000f8545  movzx eax, bl
cpu.c0t0 running
```

| Command | |
|---|---|
| `threads`, `thread NAME` | list threads, select one |
| `status`, `halt`, `go` | run state |
| `step`, `trace N`, `cont [MS]`, `wait [MS]` | single-step, step N times with disassembly, resume and wait for a stop |
| `bp SLOT ADDR [x\|w\|rw\|io] [LEN]`, `bpc SLOT` | hardware breakpoints |
| `regs`, `reg NAME [VALUE]` | registers |
| `md ADDR [LEN]`, `mw ADDR VALUE [SIZE]` | physical memory |
| `vmd`, `vmw`, `v2p`, `dis LIN [LEN]` | linear memory, translation, disassembly |
| `ior PORT [SIZE]`, `iow PORT VALUE [SIZE]` | I/O ports |
| `rdmsr INDEX`, `wrmsr INDEX VALUE` | MSRs (only MSRs known to exist, unless `force` is added) |
| `csr DEV FN OFF [SIZE] [VAL]`, `mmio DEV FN BAR OFF [SIZE] [VAL]` | uncore CSR / MMIO |
| `pins`, `preq on\|off`, `reset` | debug pins, platform reset |
| `scan TAP IR N:VALUE`, `idcodes` | raw TAP access |
| `attach`, `forcego`, `detach`, `quit` | session control |

On exit, the shell removes its breakpoints and resumes every thread it
halted, unless `detach` is used. A halted thread's saved state survives the
session, so a later `attach` can resume it correctly.

## Documentation

* [docs/protocol.md](docs/protocol.md): the DCI DbC wire protocol.
* [docs/models.md](docs/models.md): TAP networks, links, registers and
  preconditions; the `kbl-spt` platform.
* [docs/runcontrol.md](docs/runcontrol.md): run control, Lakemont and
  big-core probe mode.

## Layout

| Path | |
|---|---|
| `cmd/dcitool` | command-line tool |
| `pkg/dci` | DCI DbC transport |
| `pkg/jtag` | TAP state machine and scan builder |
| `pkg/model` | types for platform descriptions |
| `pkg/tapnet` | named TAP/register access, preconditions |
| `pkg/platforms` | platform descriptions |
| `pkg/runctl` | run control; `lakemont` for CSME and ISH cores, `bigcore` for Kaby Lake cores |
