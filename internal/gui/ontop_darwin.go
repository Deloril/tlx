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

// beginMove hands the current mouse-down event to the window so Cocoa runs its
// own drag loop — the supported way to move a borderless window by a custom bar.
static void beginMove(unsigned long nsWindow) {
	NSWindow *w = (NSWindow *)nsWindow;
	if (w == nil) {
		return;
	}
	NSEvent *e = [NSApp currentEvent];
	if (e != nil) {
		[w performWindowDragWithEvent:e];
	}
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

func nativeBeginMove(ctx any) {
	c, ok := ctx.(driver.MacWindowContext)
	if !ok {
		return
	}
	C.beginMove(C.ulong(c.NSWindow))
}
