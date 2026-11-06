# tools

Tools for capturing and decoding DCI traffic and for setting up Intel's
OpenIPC.

* `libusb-shim/`: a drop-in `libusb-1.0.so.0` for x86-64 that logs every
  bulk, control and asynchronous transfer and forwards all calls to the
  real libusb. Build with `make`, then run the program to observe with
  `LD_LIBRARY_PATH=/path/to/tools/libusb-shim USBSHIM_LOG=out.usb`.
* `decode/dcidecode.py`: turns shim logs into decoded ExI packets: config
  writes, status, TDO, and JTAG TCK streams. With `-H` it follows the TAP
  state machine and prints IR/DR operations, including pause-split scans.
* `openipc/setup.py DEST`: sets up OpenIPC and ipccli in DEST. Downloads
  them from System Studio 2020 Update 3 (Intel's server, or the Wayback
  Machine if that fails) into `~/.cache/libredci-openipc`, reusing them on
  later runs, and checks their hashes. It then decrypts
  OpenIPC's configuration (XML, schemas and Python), applies `csme.patch`
  (adds the CSME TAPs and Lakemont core on Sunrise Point-LP and Kaby Lake
  PCH), and installs ipccli with Python ≥ 3.12 fixes (`ipccli-py312.patch`)
  and IPython into a venv. Start it with `DEST/ipc`. It needs rpm2cpio,
  cpio, patch, python3-venv and python3-cryptography on an x86-64 host,
  plus the DCI udev rule from the libredci README. To change the OpenIPC
  configuration, edit the decrypted XML under `DEST/OpenIPC/Data` and
  regenerate `csme.patch`.
