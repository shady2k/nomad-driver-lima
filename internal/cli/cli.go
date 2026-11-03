package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
)

// DefaultWaitTimeout bounds the waits of `box create`, `box start` and
// `box resize`, and the default of `box wait`.
const defaultWaitTimeout = 10 * time.Minute

// App is one `box` invocation. Every dependency is a field so the unit tests
// run the real command code against fakes.
type App struct {
	// Nomad is the Nomad API. Required.
	Nomad Nomad
	// Images fetches the image index. Required for `box create` and
	// `box images`.
	Images ImageSource
	// Guest returns the exec transport for a box allocation. Required for the
	// guest commands (run, ssh, cp, status, logs, sessions).
	Guest GuestFactory
	// Terminal is the local terminal `box ssh` uses. Nil means no terminal.
	Terminal Terminal
	// Out receives the command result, ErrOut the diagnostics and prompts.
	Out, ErrOut io.Writer
	// In is the local stdin (prompt text, `box send`, `--yes` questions).
	In io.Reader
	// JSON switches the output to one JSON document.
	JSON bool
	// Now and Sleep are injectable for the tests.
	Now   func() time.Time
	Sleep func(ctx context.Context, d time.Duration) error

	// indexURL is the image index location; main sets it from the
	// environment, the tests set it directly.
	indexURL string
	// vaultRole is the Vault role a box reads its secrets with; main sets it from
	// BOX_VAULT_ROLE, the tests set it directly. Empty means the cluster's default
	// role, which is what a job with no role gets.
	vaultRole string
}

// New returns an App with the production defaults filled in.
func New(n Nomad, images ImageSource, guest GuestFactory) *App {
	return &App{
		Nomad:  n,
		Images: images,
		Guest:  guest,
		Out:    io.Discard,
		ErrOut: io.Discard,
		Now:    time.Now,
		Sleep:  sleepContext,
	}
}

func sleepContext(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// ImageIndexURL reports the index location this App reads, or "" when none is
// configured. There is no built-in default on purpose: an image index belongs to
// whoever publishes images, and this project publishes none.
func (a *App) ImageIndexURL() string {
	return a.indexURL
}

// SetImageIndexURL sets the image index location; an empty value is ignored.
func (a *App) SetImageIndexURL(url string) {
	if strings.TrimSpace(url) != "" {
		a.indexURL = strings.TrimSpace(url)
	}
}

// requireImageIndexURL returns the index location, or the one message that says
// what a missing one means. Every command that needs the index goes through it,
// so the wording cannot drift apart.
func (a *App) requireImageIndexURL() (string, error) {
	if url := a.ImageIndexURL(); url != "" {
		return url, nil
	}
	return "", errorf("the image index is not configured: set BOX_IMAGE_INDEX, or pass --image-index URL, " +
		"to the index.json of a store that publishes box images (`box images` reads it; this project " +
		"publishes none)")
}

// VaultRole reports the Vault role the CLI gives a box job, or "" for the
// cluster's default role.
func (a *App) VaultRole() string {
	return a.vaultRole
}

// SetVaultRole sets the Vault role; an empty value is ignored, like the image
// index, so an unset variable leaves the cluster's default role in place.
func (a *App) SetVaultRole(role string) {
	if role = strings.TrimSpace(role); role != "" {
		a.vaultRole = role
	}
}

// vaultRoleFor returns the role a rebuilt box job should carry: the one the job
// already has, else the configured default.
//
// A start or a resize must not drop the role its box was created with, and a box
// created before the cluster had a template item (or before BOX_VAULT_ROLE was
// set) picks the role up here, which is the same moment it picks up a newly
// staged credential.
func (a *App) vaultRoleFor(current string) string {
	if current = strings.TrimSpace(current); current != "" {
		return current
	}
	return a.VaultRole()
}

func (a *App) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}

func (a *App) sleep(ctx context.Context, d time.Duration) error {
	if a.Sleep != nil {
		return a.Sleep(ctx, d)
	}
	return sleepContext(ctx, d)
}

// command is one subcommand.
type command struct {
	// run executes the command; args are the arguments after the command name.
	run func(ctx context.Context, args []string) error
	// usage is the one-line help shown by `box help`.
	usage string
}

// Run executes one command line and returns the process exit code.
func (a *App) Run(ctx context.Context, args []string) int {
	err := a.run(ctx, args)
	if err == nil {
		return ExitOK
	}
	// An Error without a message is a guest exit code that passed straight
	// through: the guest already wrote whatever it had to say.
	var silent *Error
	if errors.As(err, &silent) && silent.Msg == "" && silent.Err == nil {
		return silent.Code
	}
	if msg := err.Error(); msg != "" {
		fmt.Fprintf(a.ErrOut, "box: %s\n", msg)
	}
	return ExitCode(err)
}

func (a *App) run(ctx context.Context, args []string) error {
	// Global flags may come before or after the command name.
	args, err := a.takeGlobalFlags(args)
	if err != nil {
		return err
	}
	if len(args) == 0 {
		a.printUsage()
		return usagef("no command given")
	}
	switch args[0] {
	case "help", "-h", "--help":
		a.printUsage()
		return nil
	}
	cmd, ok := a.commands()[args[0]]
	if !ok {
		a.printUsage()
		return usagef("unknown command %q", args[0])
	}
	return cmd.run(ctx, args[1:])
}

// takeGlobalFlags removes the global flags from anywhere on the command line,
// up to a "--": everything after it belongs to the command that follows
// (`box run <id> -- df -h /` must give df its -h, not the help command).
func (a *App) takeGlobalFlags(args []string) ([]string, error) {
	out := make([]string, 0, len(args))
	afterSeparator := false
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if afterSeparator {
			out = append(out, arg)
			continue
		}
		switch {
		case arg == "--":
			afterSeparator = true
			out = append(out, arg)
		case arg == "--json" || arg == "-json":
			a.JSON = true
		case arg == "--help" || arg == "-h":
			out = append(out, "help")
		case arg == "--image-index":
			if i+1 >= len(args) {
				return nil, usagef("--image-index needs a URL")
			}
			a.SetImageIndexURL(args[i+1])
			i++
		case strings.HasPrefix(arg, "--image-index="):
			value := strings.TrimPrefix(arg, "--image-index=")
			if strings.TrimSpace(value) == "" {
				return nil, usagef("--image-index needs a URL")
			}
			a.SetImageIndexURL(value)
		default:
			out = append(out, arg)
		}
	}
	return out, nil
}

func (a *App) printUsage() {
	names := make([]string, 0, len(a.commands()))
	for name := range a.commands() {
		names = append(names, name)
	}
	sort.Strings(names)
	fmt.Fprintf(a.ErrOut, "box - run boxes on a Nomad cluster\n\n")
	fmt.Fprintf(a.ErrOut, "usage: box [--json] [--image-index URL] <command> [arguments]\n\n")
	fmt.Fprintf(a.ErrOut, "commands:\n")
	for _, name := range names {
		fmt.Fprintf(a.ErrOut, "  %-18s %s\n", name, a.commands()[name].usage)
	}
	fmt.Fprintf(a.ErrOut, "\nexit codes: 0 ok, 1 error, 2 usage, 3 not found, 4 timeout (still running)\n")
}

func (a *App) commands() map[string]command {
	return map[string]command{
		"capacity":  {usage: "nodes that can run boxes, with their free CPU, memory and disk", run: a.cmdCapacity},
		"executors": {usage: "executor credentials of the boxes: box executors set --from-dir DIR --provider NAME | show | delete", run: a.cmdExecutors},
		"images":    {usage: "base images published for boxes", run: a.cmdImages},
		"create":    {usage: "create a box: box create <id> --image <version> --cpus N --memory-mib N --disk-gib N", run: a.cmdCreate},
		"ls":        {usage: "list boxes", run: a.cmdList},
		"status":    {usage: "details of one box, its recent task events, the guest and its sessions: box status <id>", run: a.cmdStatus},
		"stop":      {usage: "stop a box, keeping its disk: box stop <id>", run: a.cmdStop},
		"start":     {usage: "start a stopped box, with the same disk: box start <id>", run: a.cmdStart},
		"rm":        {usage: "delete a box and its disk: box rm <id> [--yes]", run: a.cmdRemove},
		"resize":    {usage: "change cpus, memory or disk: box resize <id> [--cpus N] [--memory-mib N] [--disk-gib N]", run: a.cmdResize},
		"ssh":       {usage: "interactive shell in a box: box ssh <id>", run: a.cmdSSH},
		"run":       {usage: "run a command in a box: box run <id> -- cmd...", run: a.cmdRun},
		"cp":        {usage: "copy files or directories in and out: box cp <src> <dst>", run: a.cmdCP},
		"logs":      {usage: "task logs of a box (the boxd event stream): box logs <id> [-f]", run: a.cmdLogs},
		"session":   {usage: "sessions in a box: box session new|ls|close <id> ...", run: a.cmdSession},
		"send":      {usage: "send a prompt to a session: box send <id> <session> [--file F | text]", run: a.cmdSend},
		"wait":      {usage: "wait for a session turn to finish: box wait <id> <session> [--timeout D]", run: a.cmdWait},
		"events":    {usage: "session events: box events <id> <session> [--since N] [-f]", run: a.cmdEvents},
		"cancel":    {usage: "cancel the running turn of a session: box cancel <id> <session>", run: a.cmdCancel},
	}
}
