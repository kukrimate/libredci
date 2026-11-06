# tools

Tools for capturing and decoding DCI traffic and for setting up Intel's
OpenIPC.

* `libusb-shim/`: a drop-in `libusb-1.0.so.0` for x86-64 that logs every
  bulk, control and asynchronous transfer and forwards all calls to the
  real libusb. Build with `make`, then run the program to observe with
  `LD_LIBRARY_PATH=/path/to/tools/libusb-shim USBSHIM_LOG=out.usb`.
