package cli

// Reaching root in a box.
//
// boxd runs as root, so the executors and everything an agent makes -- the files
// in /work, the tools it installs in its HOME -- belong to root. The CLI reaches
// the guest as the box's login user, which is a different user with a different
// HOME and a different PATH. `box run --executor` and a copy into or out of a
// place that user cannot reach go through `sudo -n`; these are the two things
// they share: the one guest call that proves passwordless sudo works, and the
// sentence for a box that has none.

import (
	"bytes"
	"context"
	"fmt"
	"io"
)

// boxctlExecCommand is the guest command line that runs command in an executor's
// environment: `boxctl exec` builds the environment the executor's sessions get
// (internal/boxd/exec.go), and `sudo -n` is what makes the command boxd's user,
// which is whose files /work and the executor homes are.
func boxctlExecCommand(executor, cwd string, command []string) []string {
	argv := []string{"sudo", "-n", boxctlPath, "exec", "--executor", executor}
	if cwd != "" {
		argv = append(argv, "--cwd", cwd)
	}
	return append(append(argv, "--"), command...)
}

// requireSudo proves that the box's login user may run a command as root
// without a password. A box gives that user the rule in its image; a box without
// it answers every `sudo -n` with an error of its own, which would read as the
// failure of a command that never ran.
func (a *App) requireSudo(ctx context.Context, tr ExecTransport, what string) error {
	var stderr bytes.Buffer
	code, err := tr.Exec(ctx, ExecOpts{
		Command: []string{"sudo", "-n", "true"},
		Stdout:  io.Discard,
		Stderr:  &stderr,
	})
	if err != nil {
		return wrapf(err, "cannot ask the box for root to run %s", what)
	}
	if code == 0 {
		return nil
	}
	detail := oneLine(stderr.String(), 200)
	if detail == "" {
		detail = fmt.Sprintf("sudo -n exited with code %d", code)
	}
	return sudoUnavailable(what, detail)
}

// sudoUnavailable is the sentence for a box whose login user has no passwordless
// sudo: what needed root, what the box answered, and what is left to do.
func sudoUnavailable(what, detail string) error {
	return errorf("%s needs root in the box, and the box's login user has no passwordless sudo: %s. "+
		"Every image this project builds gives that user the rule; without it, use what the user owns: "+
		"a directory it owns for box cp, and a box run without --executor", what, detail)
}
