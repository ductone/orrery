//go:build !unix

package tui

import "os"

func ownedByCurrentUser(os.FileInfo) bool { return true }
