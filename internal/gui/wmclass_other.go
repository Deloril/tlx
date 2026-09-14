//go:build !linux

package gui

// nativeSetWMClass is a no-op off Linux. macOS and Windows tie an app to its
// icon through the bundle/executable metadata, not an X11 WM_CLASS.
func nativeSetWMClass(ctx any, class string) {}
