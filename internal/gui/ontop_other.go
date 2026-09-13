//go:build !darwin && !linux && !windows

package gui

// nativeSetOnTop is a no-op on platforms without an always-on-top implementation.
func nativeSetOnTop(ctx any, on bool) {}
