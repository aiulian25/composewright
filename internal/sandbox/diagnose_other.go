//go:build !unix

package sandbox

import "io/fs"

func canOpen(string) bool { return true }

func groupOf(fs.FileInfo) int { return unknownGroup }
