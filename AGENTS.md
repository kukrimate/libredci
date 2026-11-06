# AGENTS.md

libredci is Go host software for debugging Intel platforms through DCI
(Direct Connect Interface) over USB DbC. `README.md` covers features, usage
and layout; `docs/` follows the package layout: the wire protocol
(`protocol.md`), the model concepts and chip detection (`model.md`), one
file per chip (`pch/`, `cpu/`) and per building block (`ip/`), and run
control (`runctl.md`, with the backends in `runctl/`).

## Build and test

```
go build ./cmd/dcitool
go vet ./...
go test ./...
gofmt -l .
```

Needs Go and the libusb-1.0 development files. Tests run without hardware.

Python in `tools/` must pass `black --check` and `pylint` with no messages.
The libusb shim builds with `make` in `tools/libusb-shim`.

## Hardware

Commands that reach a target (`dcitool` scans, the shell, `tools/openipc`)
need a DCI-enabled target on USB. Only run them when the user asks.
Raw-mode scans (`dcitool -chain`) and register writes act on the real
target: a wrong IR length or value writes to arbitrary target registers.

`tools/openipc/setup.py DEST` sets up Intel's OpenIPC and ipccli. Its
output is a reference for comparing behaviour; it is not part of the
repository. Don't commit Intel binaries, RPMs or decrypted OpenIPC files;
changes to OpenIPC's configuration go into `tools/openipc/csme.patch`.

## Code

* Comments, docstrings, docs and commit messages say what the code does.
  Leave out motivation, history and backstory.
* Every source file (Go, C, Python, shell, Makefile) starts with
  `SPDX-License-Identifier: GPL-2.0-only`, after any `#!` line. In Go, a
  blank line separates it from the package comment. Patch files are not
  tagged.
* Scripts are Python 3, standard library where possible, not shell.
* One code path: depend on one named tool and list it as a requirement
  instead of detecting alternatives or adding fallbacks.
* Keep `docs/` and the README's command table in step with code changes.
* `tools/` targets x86-64 Linux hosts.

## Commits

* Linear history is preferred. Each commit is one self-contained change
  that builds and passes `go vet` and `go test`.
* Fold fixes into earlier commits only while those commits are unpublished
  work in progress. Never rewrite history that is on a public branch (for
  example `main` on GitHub): put fixes for it in new commits on top.
* Subject in the imperative, `area: change` for focused changes (for
  example `dci: time out bulk OUT writes`, `tools: add DCI traffic
  decoder`). The body describes what the change does.
* Commit only when the user asks.
