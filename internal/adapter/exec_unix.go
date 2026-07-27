//go:build unix

package adapter

import (
	"os"
	"os/exec"
	"syscall"
)

// setProcessGroup makes the child the leader of a new process group, so the
// per-attempt timeout can reach everything it spawned rather than just the
// script itself.
func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killProcessGroup SIGKILLs the whole group. A negative pid addresses the
// group, which is why setProcessGroup had to make the child its leader —
// without it this would signal hubbub's own group, including hubbub.
//
// SIGKILL rather than a TERM-then-KILL courtesy: by the time this runs the
// attempt deadline has already passed, delivery is at-least-once anyway, and a
// grace period is just a longer wedge on a channel whose worker is serial.
func killProcessGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return os.ErrProcessDone
	}
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
		// The group may already be gone (the script exited between the
		// deadline and this call). Fall back to the process itself so a real
		// survivor is still killed, and let Wait sort out the rest.
		return cmd.Process.Kill()
	}
	return nil
}
