//go:build linux

package sandbox

import (
	"context"
	"os/exec"
	"syscall"
)

// newRunscCommand constructs the runsc invocation matching the
// validated probe (precns-test.sh line 66) plus the systrap platform
// choice (SKY-334 benchmark — 27% faster sustained syscalls than
// ptrace on the same Fly Machine, same cold-start).
//
//	runsc --platform=systrap --ignore-cgroups --network=sandbox \
//	      run --bundle <bundleDir> <containerID>
//
// cmd.Cancel SIGKILLs the runsc parent; gVisor's supervision
// propagates the signal into the sandboxed init. No Setpgid —
// runsc manages its own process tree.
func newRunscCommand(ctx context.Context, bundleDir, containerID string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "runsc",
		"--platform=systrap",
		"--ignore-cgroups",
		"--network=sandbox",
		"run",
		"--bundle", bundleDir,
		containerID,
	)
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		// SIGKILL the runsc process itself. gVisor sandbox-init is
		// runsc's child; killing the parent tears down the sandbox.
		// ESRCH is fine — process already exited between Wait
		// returning and the cancel watcher reading ctx.Done().
		return cmd.Process.Signal(syscall.SIGKILL)
	}
	return cmd
}
