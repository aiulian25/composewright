//go:build !linux

package runas

// Prepare does nothing off Linux, where the image never runs.
func Prepare() error { return nil }
