//go:build !unix

package secrets

import "io/fs"

func checkOwner(string, fs.FileInfo) error { return nil }
