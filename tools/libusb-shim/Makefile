# SPDX-License-Identifier: GPL-2.0-only
CC ?= gcc
REAL ?= /usr/lib/$(shell $(CC) -print-multiarch)/libusb-1.0.so.0

libusb-1.0.so.0: shim.c tramp.S names.c
	$(CC) -O2 -Wall -fPIC -shared -Wl,-soname,libusb-1.0.so.0 -DUSBSHIM_DEFAULT_REAL='"$(REAL)"' -o $@ shim.c tramp.S names.c -ldl -lpthread

tramp.S names.c: gen.sh shim.c
	CC=$(CC) ./gen.sh $(REAL)

clean:
	rm -f libusb-1.0.so.0 tramp.S names.c exports.txt hooked.txt forwarded.txt
