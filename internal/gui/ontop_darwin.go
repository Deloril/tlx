//go:build darwin

package gui

/*
#cgo CFLAGS: -x objective-c
#cgo LDFLAGS: -framework Cocoa
#import <Cocoa/Cocoa.h>

// setOnTop flips a window's level. NSFloatingWindowLevel keeps it above normal
// windows; NSNormalWindowLevel returns it to the pack. Runs on the main thread
// because RunNative hands us the platform context there.
static void setOnTop(unsigned long nsWindow, int on) {
	NSWindow *w = (NSWindow *)nsWindow;
	if (w == nil) {
		return;
	}
	[w setLevel:(on ? NSFloatingWindowLevel : NSNormalWindowLevel)];
}
*/
import "C"

import "fyne.io/fyne/v2/driver"

func nativeSetOnTop(ctx any, on bool) {
	c, ok := ctx.(driver.MacWindowContext)
	if !ok {
		return
	}
	v := C.int(0)
	if on {
		v = 1
	}
	C.setOnTop(C.ulong(c.NSWindow), v)
}
