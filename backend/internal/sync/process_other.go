//go:build !unix

package sync

import "os/exec"

// isolateProcessGroup is a no-op on platforms without Unix process groups;
// exec.CommandContext's default kill behaviour still applies.
func isolateProcessGroup(cmd *exec.Cmd) {}
