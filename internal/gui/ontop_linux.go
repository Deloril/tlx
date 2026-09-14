//go:build linux

package gui

/*
#cgo LDFLAGS: -lX11
#include <X11/Xlib.h>
#include <X11/Xutil.h>
#include <string.h>
#include <stdlib.h>

// setOnTop toggles _NET_WM_STATE_ABOVE on an X11 window via an EWMH client
// message to the root window — the correct way to change state on an already
// mapped window. We open our own display connection (the window id is all we
// get from Fyne) and address the window by id, which works cross-connection.
static void setOnTop(unsigned long win, int on) {
	Display *d = XOpenDisplay(NULL);
	if (d == NULL) {
		return;
	}
	Atom wmState = XInternAtom(d, "_NET_WM_STATE", False);
	Atom above = XInternAtom(d, "_NET_WM_STATE_ABOVE", False);

	XEvent e;
	memset(&e, 0, sizeof(e));
	e.type = ClientMessage;
	e.xclient.window = (Window)win;
	e.xclient.message_type = wmState;
	e.xclient.format = 32;
	e.xclient.data.l[0] = on ? 1 : 0; // _NET_WM_STATE_ADD (1) / _REMOVE (0)
	e.xclient.data.l[1] = (long)above;
	e.xclient.data.l[2] = 0;
	e.xclient.data.l[3] = 1; // source indication: normal application

	XSendEvent(d, DefaultRootWindow(d), False,
		SubstructureRedirectMask | SubstructureNotifyMask, &e);
	XFlush(d);
	XCloseDisplay(d);
}

// setWMClass rewrites the window's ICCCM WM_CLASS (res_name and res_class) to
// the given id. GLFW only sets WM_CLASS at window-creation time, from
// $RESOURCE_NAME and the title, so it never carries our application id; without
// it a desktop environment can't match the window to the installed .desktop
// file and falls back to a generic icon. Both fields are set to the same id so
// StartupWMClass matching works whether the DE keys off the instance or the
// class.
static void setWMClass(unsigned long win, const char *id) {
	Display *d = XOpenDisplay(NULL);
	if (d == NULL) {
		return;
	}
	XClassHint *hint = XAllocClassHint();
	if (hint != NULL) {
		hint->res_name = (char *)id;
		hint->res_class = (char *)id;
		XSetClassHint(d, (Window)win, hint);
		XFree(hint);
	}
	XFlush(d);
	XCloseDisplay(d);
}
*/
import "C"

import (
	"unsafe"

	"fyne.io/fyne/v2/driver"
)

func nativeSetOnTop(ctx any, on bool) {
	c, ok := ctx.(driver.X11WindowContext)
	if !ok {
		return // Wayland has no equivalent; leave it a no-op there
	}
	v := C.int(0)
	if on {
		v = 1
	}
	C.setOnTop(C.ulong(c.WindowHandle), v)
}

func nativeSetWMClass(ctx any, class string) {
	c, ok := ctx.(driver.X11WindowContext)
	if !ok {
		return // Wayland derives app_id from the app's UniqueID; nothing to do
	}
	cs := C.CString(class)
	defer C.free(unsafe.Pointer(cs))
	C.setWMClass(C.ulong(c.WindowHandle), cs)
}
