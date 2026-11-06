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
