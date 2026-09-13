//go:build linux

package gui

/*
#cgo LDFLAGS: -lX11
#include <X11/Xlib.h>
#include <string.h>

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
*/
import "C"

import "fyne.io/fyne/v2/driver"

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
