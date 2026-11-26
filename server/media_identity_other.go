//go:build !unix

package server

import "os"

// fileIdentity has no portable spelling off unix, so the version falls back to
// size plus modification time alone. A rewrite that keeps both is then
// indistinguishable, which is the same trade the Harness makes without a
// device-and-inode stat.
func fileIdentity(os.FileInfo) string { return "0:0" }
