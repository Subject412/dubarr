//go:build unix

package sync

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

// isolateProcessGroup puts the engine in its own process group and makes
// context cancellation kill the whole group.
//
// exec.CommandContext alone only kills the direct child; a child that spawns
// helpers (or a shell wrapper) could keep the stdout pipe open and make Wait
// block past the deadline. Killing the group is also the correct behaviour for
// a wedged analysis process holding decoder children.
func isolateProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
}
