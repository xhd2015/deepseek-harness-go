//go:build unix

package server

import (
	"fmt"
	"os"
	"syscall"
)

// fileIdentity is the part of a file's identity that survives a rewrite: the
// device and inode. Two stats that agree on it are the same file even when the
// mtime granularity is too coarse to tell.
func fileIdentity(info os.FileInfo) string {
	if stat, ok := info.Sys().(*syscall.Stat_t); ok {
		return fmt.Sprintf("%d:%d", uint64(stat.Dev), uint64(stat.Ino))
	}
	return "0:0"
}
