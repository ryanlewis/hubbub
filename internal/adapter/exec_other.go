//go:build !unix

package adapter

import (
	"os"
	"os/exec"
)

// Process groups are a Unix concept, and hubbub deploys to Linux. These keep
// the package building everywhere else, with the timeout reaching the script
// but not whatever it spawned — stated here rather than discovered later.

func setProcessGroup(*exec.Cmd) {}

func killProcessGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return os.ErrProcessDone
	}
	return cmd.Process.Kill()
}
