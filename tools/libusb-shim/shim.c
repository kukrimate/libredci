// SPDX-License-Identifier: GPL-2.0-only

// libusb-shim: a drop-in libusb-1.0.so.0 that logs bulk, control and
// asynchronous transfers and forwards every call to the real libusb.
//
// Env:
//   USBSHIM_REAL  path to real libusb (default: the build host's, see Makefile)
//   USBSHIM_LOG   log file (default stderr)
#define _GNU_SOURCE
#ifndef USBSHIM_DEFAULT_REAL
#error "build with make, which sets USBSHIM_DEFAULT_REAL"
#endif
#include <dlfcn.h>
#include <pthread.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <time.h>

// ---- minimal libusb declarations ----
struct libusb_transfer;
typedef void (*libusb_transfer_cb_fn)(struct libusb_transfer *);
struct libusb_transfer {
	void *dev_handle;
	uint8_t flags;
	unsigned char endpoint;
	unsigned char type;
	unsigned int timeout;
	int status;
	int length;
	int actual_length;
	libusb_transfer_cb_fn callback;
	void *user_data;
	unsigned char *buffer;
	int num_iso_packets;
};

// trampoline table filled in by the constructor; referenced from tramp.S
__attribute__((visibility("hidden"))) void *shim_real[256];
extern const char *const shim_names[] __attribute__((visibility("hidden")));
extern const int shim_count;

static void *real_lib;
static FILE *logf;
static pthread_mutex_t loglock = PTHREAD_MUTEX_INITIALIZER;
static struct timespec t0;

static void *real(const char *name) {
	void *p = dlsym(real_lib, name);
	if (!p) {
		fprintf(stderr, "usbshim: missing %s\n", name);
		abort();
	}
	return p;
}

__attribute__((constructor)) static void shim_init(void) {
	const char *path = getenv("USBSHIM_REAL");
	if (!path)
		path = USBSHIM_DEFAULT_REAL;
	real_lib = dlopen(path, RTLD_NOW | RTLD_LOCAL);
	if (!real_lib) {
		fprintf(stderr, "usbshim: dlopen %s: %s\n", path, dlerror());
		abort();
	}
	for (int i = 0; i < shim_count; i++)
		shim_real[i] = real(shim_names[i]);
	const char *lp = getenv("USBSHIM_LOG");
	logf = lp ? fopen(lp, "a") : NULL;
	if (!logf)
		logf = stderr;
	setvbuf(logf, NULL, _IOLBF, 0);
	clock_gettime(CLOCK_MONOTONIC, &t0);
	fprintf(logf, "# usbshim loaded, real=%s\n", path);
}

static double now(void) {
	struct timespec t;
	clock_gettime(CLOCK_MONOTONIC, &t);
	return (t.tv_sec - t0.tv_sec) + (t.tv_nsec - t0.tv_nsec) / 1e9;
}

// One line per event: "<t> <tag> ep=XX len=N st=S : hex"
static void logbuf(const char *tag, int ep, int status, const unsigned char *buf, int len) {
	pthread_mutex_lock(&loglock);
	fprintf(logf, "%.6f %s ep=%02x len=%d st=%d :", now(), tag, ep & 0xff, len, status);
	for (int i = 0; i < len; i++)
		fprintf(logf, "%s%02x", (i % 4) ? "" : " ", buf[i]);
	fputc('\n', logf);
	pthread_mutex_unlock(&loglock);
}

static void logmsg(const char *fmt, ...) __attribute__((format(printf, 1, 2)));
#include <stdarg.h>
static void logmsg(const char *fmt, ...) {
	va_list ap;
	va_start(ap, fmt);
	pthread_mutex_lock(&loglock);
	fprintf(logf, "%.6f ", now());
	vfprintf(logf, fmt, ap);
	fputc('\n', logf);
	pthread_mutex_unlock(&loglock);
	va_end(ap);
}

// ---- hooked synchronous calls ----
int libusb_bulk_transfer(void *h, unsigned char ep, unsigned char *data, int len, int *xfer, unsigned int timeout) {
	static int (*f)(void *, unsigned char, unsigned char *, int, int *, unsigned int);
	if (!f) f = real("libusb_bulk_transfer");
	if (!(ep & 0x80)) logbuf("BULK_OUT", ep, 0, data, len);
	int r = f(h, ep, data, len, xfer, timeout);
	if (ep & 0x80) logbuf("BULK_IN ", ep, r, data, (r == 0 && xfer) ? *xfer : 0);
	else if (r) logmsg("BULK_OUT ep=%02x result=%d", ep, r);
	return r;
}

int libusb_interrupt_transfer(void *h, unsigned char ep, unsigned char *data, int len, int *xfer, unsigned int timeout) {
	static int (*f)(void *, unsigned char, unsigned char *, int, int *, unsigned int);
	if (!f) f = real("libusb_interrupt_transfer");
	if (!(ep & 0x80)) logbuf("INTR_OUT", ep, 0, data, len);
	int r = f(h, ep, data, len, xfer, timeout);
	if (ep & 0x80) logbuf("INTR_IN ", ep, r, data, (r == 0 && xfer) ? *xfer : 0);
	return r;
}

int libusb_control_transfer(void *h, uint8_t rt, uint8_t req, uint16_t val, uint16_t idx, unsigned char *data, uint16_t len, unsigned int timeout) {
	static int (*f)(void *, uint8_t, uint8_t, uint16_t, uint16_t, unsigned char *, uint16_t, unsigned int);
	if (!f) f = real("libusb_control_transfer");
	int r = f(h, rt, req, val, idx, data, len, timeout);
	pthread_mutex_lock(&loglock);
	fprintf(logf, "%.6f CTRL rt=%02x req=%02x val=%04x idx=%04x len=%u r=%d :", now(), rt, req, val, idx, len, r);
	int n = (rt & 0x80) ? (r > 0 ? r : 0) : len;
	for (int i = 0; i < n; i++) fprintf(logf, " %02x", data[i]);
	fputc('\n', logf);
	pthread_mutex_unlock(&loglock);
	return r;
}

// ---- async transfers: wrap the completion callback ----
#define MAXX 1024
static struct { struct libusb_transfer *t; libusb_transfer_cb_fn cb; } xmap[MAXX];
static pthread_mutex_t xlock = PTHREAD_MUTEX_INITIALIZER;

static void shim_cb(struct libusb_transfer *t) {
	libusb_transfer_cb_fn cb = NULL;
	pthread_mutex_lock(&xlock);
	for (int i = 0; i < MAXX; i++)
		if (xmap[i].t == t) { cb = xmap[i].cb; xmap[i].t = NULL; break; }
	pthread_mutex_unlock(&xlock);
	if (t->endpoint & 0x80)
		logbuf("ASYNC_IN ", t->endpoint, t->status, t->buffer, t->actual_length);
	else
		logmsg("ASYNC_OUT_DONE ep=%02x st=%d actual=%d", t->endpoint, t->status, t->actual_length);
	t->callback = cb;
	if (cb) cb(t);
}

int libusb_submit_transfer(struct libusb_transfer *t) {
	static int (*f)(struct libusb_transfer *);
	if (!f) f = real("libusb_submit_transfer");
	if (!(t->endpoint & 0x80)) {
		if (t->type == 0) // control: setup packet + data
			logbuf("ASYNC_CTRL", t->endpoint, 0, t->buffer, t->length);
		else
			logbuf("ASYNC_OUT", t->endpoint, 0, t->buffer, t->length);
	} else {
		logmsg("ASYNC_IN_SUBMIT ep=%02x len=%d", t->endpoint, t->length);
	}
	if (t->callback != shim_cb) {
		pthread_mutex_lock(&xlock);
		int i;
		for (i = 0; i < MAXX; i++)
			if (!xmap[i].t) { xmap[i].t = t; xmap[i].cb = t->callback; break; }
		pthread_mutex_unlock(&xlock);
		if (i < MAXX) t->callback = shim_cb;
		else logmsg("usbshim: transfer map full");
	}
	int r = f(t);
	if (r) {
		logmsg("SUBMIT_FAIL ep=%02x r=%d", t->endpoint, r);
		pthread_mutex_lock(&xlock);
		for (int i = 0; i < MAXX; i++)
			if (xmap[i].t == t) { t->callback = xmap[i].cb; xmap[i].t = NULL; break; }
		pthread_mutex_unlock(&xlock);
	}
	return r;
}

int libusb_cancel_transfer(struct libusb_transfer *t) {
	static int (*f)(struct libusb_transfer *);
	if (!f) f = real("libusb_cancel_transfer");
	logmsg("CANCEL ep=%02x", t->endpoint);
	return f(t);
}

// ---- logged device-level calls ----
#define HOOK1(name, T1, fmt)                                       \
	int name(void *h, T1 a) {                                      \
		static int (*f)(void *, T1);                               \
		if (!f) f = real(#name);                                   \
		int r = f(h, a);                                           \
		logmsg(#name "(" fmt ") = %d", a, r);                      \
		return r;                                                  \
	}
HOOK1(libusb_claim_interface, int, "%d")
HOOK1(libusb_release_interface, int, "%d")
HOOK1(libusb_set_configuration, int, "%d")
HOOK1(libusb_clear_halt, unsigned char, "0x%02x")
HOOK1(libusb_detach_kernel_driver, int, "%d")

int libusb_set_interface_alt_setting(void *h, int iface, int alt) {
	static int (*f)(void *, int, int);
	if (!f) f = real("libusb_set_interface_alt_setting");
	int r = f(h, iface, alt);
	logmsg("libusb_set_interface_alt_setting(%d,%d) = %d", iface, alt, r);
	return r;
}

int libusb_reset_device(void *h) {
	static int (*f)(void *);
	if (!f) f = real("libusb_reset_device");
	int r = f(h);
	logmsg("libusb_reset_device() = %d", r);
	return r;
}

int libusb_open(void *dev, void **h) {
	static int (*f)(void *, void **);
	if (!f) f = real("libusb_open");
	int r = f(dev, h);
	logmsg("libusb_open() = %d", r);
	return r;
}

void libusb_close(void *h) {
	static void (*f)(void *);
	if (!f) f = real("libusb_close");
	logmsg("libusb_close()");
	f(h);
}
