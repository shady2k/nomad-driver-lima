package boxd

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

// DefaultSocket is where boxd listens in the guest.
const DefaultSocket = "/run/boxd/boxd.sock"

// BoxctlUsage is printed to stderr for a bad invocation.
const BoxctlUsage = `usage:
  boxctl info
  boxctl session new --executor prime|opencode|claude --model <provider>/<model> [--cwd DIR] [--title T]
  boxctl session ls
  boxctl session get <id>
  boxctl session close <id>
  boxctl send <id>            (prompt text on stdin)
  boxctl wait <id> [--turn N] [--timeout SEC]
  boxctl events <id> [--since SEQ] [--limit N]
  boxctl cancel <id>
  boxctl reload              (re-read the executor credentials, keep the sessions)
  boxctl prepare start --dest DIR [--repo URL [--ref REF] | --files] [--limit SEC]
                             (--files: a tar stream on stdin; neither: the recipe again)
  boxctl prepare get | wait [--timeout SEC] | cancel | skip
  boxctl seal --name NAME [--version V] [--parent V] [--arch amd64|arm64] [--recipe FILE]
                             (seals the box into an image; --recipe - reads it from stdin,
                              and a seal prints one {"line":...} per step, then {"seal":...})

global: --socket PATH (default $BOXD_SOCKET or ` + DefaultSocket + `)
Every command prints one JSON document on stdout and exits 0, or prints
{"error":{"code":...,"message":...}} and exits 1. Diagnostics go to stderr.
`

// RunBoxctl executes one boxctl command and returns the process exit code.
func RunBoxctl(args []string, in io.Reader, out, errw io.Writer) int {
	ctx := context.Background()
	socket, rest, err := takeGlobalFlags(args)
	if err != nil {
		return printError(out, errw, err)
	}
	c := NewClient(socket)

	if len(rest) == 0 {
		fmt.Fprint(errw, BoxctlUsage)
		return printError(out, errw, Errorf(CodeInvalid, "no command given"))
	}
	switch rest[0] {
	case "help", "-h", "--help":
		fmt.Fprint(errw, BoxctlUsage)
		return 0
	case "info":
		return cmdInfo(ctx, c, rest[1:], out, errw)
	case "session":
		if len(rest) < 2 {
			return printError(out, errw, Errorf(CodeInvalid, "session needs a subcommand (new, ls, get, close)"))
		}
		switch rest[1] {
		case "new":
			return cmdSessionNew(ctx, c, rest[2:], out, errw)
		case "ls":
			return cmdSessionLs(ctx, c, rest[2:], out, errw)
		case "get":
			return cmdSessionGet(ctx, c, rest[2:], out, errw)
		case "close":
			return cmdSessionClose(ctx, c, rest[2:], out, errw)
		}
		return printError(out, errw, Errorf(CodeInvalid, "unknown session subcommand %q", rest[1]))
	case "send":
		return cmdSend(ctx, c, rest[1:], in, out, errw)
	case "wait":
		return cmdWait(ctx, c, rest[1:], out, errw)
	case "events":
		return cmdEvents(ctx, c, rest[1:], out, errw)
	case "cancel":
		return cmdCancel(ctx, c, rest[1:], out, errw)
	case "reload":
		return cmdReload(ctx, c, rest[1:], out, errw)
	case "prepare":
		return cmdPrepare(ctx, c, rest[1:], in, out, errw)
	case "seal":
		return cmdSeal(ctx, c, rest[1:], in, out, errw)
	}
	return printError(out, errw, Errorf(CodeInvalid, "unknown command %q", rest[0]))
}

// takeGlobalFlags pulls --socket (and -socket) out of the argument list wherever
// it appears, so "boxctl session new --socket X ..." works like the drivers use it.
func takeGlobalFlags(args []string) (socket string, rest []string, err error) {
	socket = os.Getenv("BOXD_SOCKET")
	if socket == "" {
		socket = DefaultSocket
	}
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--socket" || a == "-socket":
			if i+1 >= len(args) {
				return "", nil, Errorf(CodeInvalid, "%s needs a path", a)
			}
			socket = args[i+1]
			i++
		case strings.HasPrefix(a, "--socket="):
			socket = strings.TrimPrefix(a, "--socket=")
		case strings.HasPrefix(a, "-socket="):
			socket = strings.TrimPrefix(a, "-socket=")
		default:
			rest = append(rest, a)
		}
	}
	return socket, rest, nil
}

func printError(out, errw io.Writer, err error) int {
	e := asError(err)
	if e.Code == CodeInvalid {
		fmt.Fprintln(errw, e.Message)
	}
	b, _ := json.Marshal(ErrorResponse{Error: ErrorBody{Code: e.Code, Message: e.Message}})
	fmt.Fprintln(out, string(b))
	return 1
}

func printJSON(out io.Writer, v any) int {
	b, err := json.Marshal(v)
	if err != nil {
		return 1
	}
	fmt.Fprintln(out, string(b))
	return 0
}

// cmdReload asks boxd to re-read the executor credentials.
func cmdReload(ctx context.Context, c *Client, args []string, out, errw io.Writer) int {
	if err := noArgs(args); err != nil {
		return printError(out, errw, err)
	}
	res, err := c.Reload(ctx)
	if err != nil {
		return printError(out, errw, err)
	}
	return printJSON(out, res)
}

func cmdInfo(ctx context.Context, c *Client, args []string, out, errw io.Writer) int {
	if err := noArgs(args); err != nil {
		return printError(out, errw, err)
	}
	info, err := c.Info(ctx)
	if err != nil {
		return printError(out, errw, err)
	}
	return printJSON(out, info)
}

func cmdSessionNew(ctx context.Context, c *Client, args []string, out, errw io.Writer) int {
	fs := flag.NewFlagSet("session new", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	executor := fs.String("executor", "prime", "executor name")
	model := fs.String("model", "", "model as <provider>/<model>")
	cwd := fs.String("cwd", "", "working directory")
	title := fs.String("title", "", "session title")
	if err := fs.Parse(args); err != nil {
		return printError(out, errw, Errorf(CodeInvalid, "session new: %v", err))
	}
	if fs.NArg() > 0 {
		return printError(out, errw, Errorf(CodeInvalid, "unexpected argument %q", fs.Arg(0)))
	}
	if *model == "" {
		return printError(out, errw, Errorf(CodeInvalid, "session new needs --model <provider>/<model>"))
	}
	id, err := c.NewSession(ctx, *executor, *model, *cwd, *title)
	if err != nil {
		return printError(out, errw, err)
	}
	return printJSON(out, map[string]string{"session": id})
}

func cmdSessionLs(ctx context.Context, c *Client, args []string, out, errw io.Writer) int {
	if err := noArgs(args); err != nil {
		return printError(out, errw, err)
	}
	sessions, err := c.Sessions(ctx)
	if err != nil {
		return printError(out, errw, err)
	}
	if sessions == nil {
		sessions = []Session{}
	}
	return printJSON(out, map[string]any{"sessions": sessions})
}

func cmdSessionGet(ctx context.Context, c *Client, args []string, out, errw io.Writer) int {
	id, err := oneID(args)
	if err != nil {
		return printError(out, errw, err)
	}
	s, err := c.Session(ctx, id)
	if err != nil {
		return printError(out, errw, err)
	}
	return printJSON(out, s)
}

func cmdSessionClose(ctx context.Context, c *Client, args []string, out, errw io.Writer) int {
	id, err := oneID(args)
	if err != nil {
		return printError(out, errw, err)
	}
	if err := c.Close(ctx, id); err != nil {
		return printError(out, errw, err)
	}
	return printJSON(out, map[string]string{"session": id, "state": StateClosed})
}

func cmdSend(ctx context.Context, c *Client, args []string, in io.Reader, out, errw io.Writer) int {
	id, err := oneID(args)
	if err != nil {
		return printError(out, errw, err)
	}
	prompt, err := io.ReadAll(io.LimitReader(in, 4<<20))
	if err != nil {
		return printError(out, errw, Errorf(CodeInvalid, "read prompt from stdin: %v", err))
	}
	if strings.TrimSpace(string(prompt)) == "" {
		return printError(out, errw, Errorf(CodeInvalid, "send needs the prompt on stdin"))
	}
	turn, err := c.Send(ctx, id, string(prompt))
	if err != nil {
		return printError(out, errw, err)
	}
	return printJSON(out, map[string]any{"session": id, "turn": turn})
}

func cmdWait(ctx context.Context, c *Client, args []string, out, errw io.Writer) int {
	id, rest, err := splitID(args)
	if err != nil {
		return printError(out, errw, err)
	}
	fs := flag.NewFlagSet("wait", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	turn := fs.Int("turn", 0, "turn number (0 = the latest)")
	timeout := fs.Float64("timeout", 300, "seconds to wait")
	if err := fs.Parse(rest); err != nil {
		return printError(out, errw, Errorf(CodeInvalid, "wait: %v", err))
	}
	if fs.NArg() > 0 {
		return printError(out, errw, Errorf(CodeInvalid, "unexpected argument %q", fs.Arg(0)))
	}
	if *timeout < 0 {
		return printError(out, errw, Errorf(CodeInvalid, "timeout must not be negative"))
	}
	waitFor := time.Duration(*timeout * float64(time.Second))
	callCtx := ctx
	var cancel context.CancelFunc
	if waitFor > 0 {
		callCtx, cancel = context.WithTimeout(ctx, waitFor+30*time.Second)
		defer cancel()
	}
	t, err := c.Wait(callCtx, id, *turn, waitFor)
	if err != nil {
		return printError(out, errw, err)
	}
	return printJSON(out, t)
}

func cmdEvents(ctx context.Context, c *Client, args []string, out, errw io.Writer) int {
	id, rest, err := splitID(args)
	if err != nil {
		return printError(out, errw, err)
	}
	fs := flag.NewFlagSet("events", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	since := fs.Int64("since", 0, "return events with seq > since")
	limit := fs.Int("limit", 100, "maximum number of events")
	if err := fs.Parse(rest); err != nil {
		return printError(out, errw, Errorf(CodeInvalid, "events: %v", err))
	}
	if fs.NArg() > 0 {
		return printError(out, errw, Errorf(CodeInvalid, "unexpected argument %q", fs.Arg(0)))
	}
	page, err := c.Events(ctx, id, *since, *limit)
	if err != nil {
		return printError(out, errw, err)
	}
	if page.Events == nil {
		page.Events = []Event{}
	}
	if page.More {
		// Say that this is a page and not the feed: a caller that reads the
		// limit as the end of the events takes a busy session for a quiet one.
		fmt.Fprintf(errw, "%d of %d events, more with --since %d\n", len(page.Events), page.Total, page.Next)
	}
	return printJSON(out, map[string]any{"events": page.Events, "next": page.Next, "total": page.Total, "more": page.More})
}

func cmdCancel(ctx context.Context, c *Client, args []string, out, errw io.Writer) int {
	id, err := oneID(args)
	if err != nil {
		return printError(out, errw, err)
	}
	t, err := c.Cancel(ctx, id)
	if err != nil {
		return printError(out, errw, err)
	}
	return printJSON(out, t)
}

func noArgs(args []string) error {
	if len(args) > 0 {
		return Errorf(CodeInvalid, "unexpected argument %q", args[0])
	}
	return nil
}

func oneID(args []string) (string, error) {
	id, rest, err := splitID(args)
	if err != nil {
		return "", err
	}
	if len(rest) > 0 {
		return "", Errorf(CodeInvalid, "unexpected argument %q", rest[0])
	}
	return id, nil
}

// splitID takes the leading non-flag argument as a session id.
func splitID(args []string) (string, []string, error) {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return "", nil, Errorf(CodeInvalid, "a session id is required")
	}
	return args[0], args[1:], nil
}

// cmdPrepare drives the box's preparation (prepare.go). The sources belong to
// the user boxctl runs as: the one `box run` and `box cp` use.
func cmdPrepare(ctx context.Context, c *Client, args []string, in io.Reader, out, errw io.Writer) int {
	if len(args) == 0 {
		return printError(out, errw, Errorf(CodeInvalid, "prepare needs a subcommand (start, get, wait, cancel, skip)"))
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "start":
		fs := flag.NewFlagSet("prepare start", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		dest := fs.String("dest", "", "where the sources go in the box")
		repo := fs.String("repo", "", "git repository to clone")
		ref := fs.String("ref", "", "branch, tag or commit to check out")
		files := fs.Bool("files", false, "the sources are a tar stream on stdin")
		limit := fs.Int("limit", 0, "time limit of the preparation in seconds")
		if err := fs.Parse(rest); err != nil {
			return printError(out, errw, Errorf(CodeInvalid, "prepare start: %v", err))
		}
		if *files && *repo != "" {
			return printError(out, errw, Errorf(CodeInvalid, "prepare start: --repo and --files are two sources, give one"))
		}
		req := PrepareRequest{Repo: *repo, Ref: *ref, Dest: *dest, LimitSec: *limit,
			Owner: fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid())}
		var (
			st  PrepareState
			err error
		)
		if *files {
			st, err = c.PrepareFiles(ctx, req, in)
		} else {
			st, err = c.Prepare(ctx, req)
		}
		if err != nil {
			return printError(out, errw, err)
		}
		return printJSON(out, st)
	case "get":
		st, err := c.PrepareState(ctx)
		if err != nil {
			return printError(out, errw, err)
		}
		return printJSON(out, st)
	case "wait":
		fs := flag.NewFlagSet("prepare wait", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		timeout := fs.Int("timeout", 30, "seconds to wait")
		if err := fs.Parse(rest); err != nil {
			return printError(out, errw, Errorf(CodeInvalid, "prepare wait: %v", err))
		}
		callCtx, cancel := context.WithTimeout(ctx, time.Duration(*timeout+30)*time.Second)
		defer cancel()
		st, err := c.PrepareWait(callCtx, time.Duration(*timeout)*time.Second)
		if err != nil {
			return printError(out, errw, err)
		}
		return printJSON(out, st)
	case "cancel":
		st, err := c.PrepareCancel(ctx)
		if err != nil {
			return printError(out, errw, err)
		}
		return printJSON(out, st)
	case "skip":
		st, err := c.PrepareSkip(ctx)
		if err != nil {
			return printError(out, errw, err)
		}
		return printJSON(out, st)
	}
	return printError(out, errw, Errorf(CodeInvalid, "unknown prepare subcommand %q", sub))
}

// cmdSeal seals the box into an image.
//
// The recipe is the script that makes this environment from the parent image, so
// it takes the bytes of a script and not a JSON string: --recipe FILE reads a file
// in the guest, --recipe - reads stdin (which is how the CLI on the host hands it
// over without a second round trip).
func cmdSeal(ctx context.Context, c *Client, args []string, in io.Reader, out, errw io.Writer) int {
	fs := flag.NewFlagSet("seal", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	name := fs.String("name", "", "the image's name")
	version := fs.String("version", "", "the image's version (recorded in /etc/box/image.json)")
	parent := fs.String("parent", "", "the image the box came from")
	arch := fs.String("arch", "", "the image's architecture: amd64 or arm64")
	recipe := fs.String("recipe", "", "the recipe that makes this environment: a file, or - for stdin")
	if err := fs.Parse(args); err != nil {
		return printError(out, errw, Errorf(CodeInvalid, "seal: %v", err))
	}
	if fs.NArg() > 0 {
		return printError(out, errw, Errorf(CodeInvalid, "unexpected argument %q", fs.Arg(0)))
	}
	if *name == "" {
		return printError(out, errw, Errorf(CodeInvalid, "seal needs --name: the image is named after the box's project"))
	}
	req := SealRequest{Name: *name, Version: *version, Parent: *parent, Arch: *arch}
	if *recipe != "" {
		var data []byte
		var err error
		if *recipe == "-" {
			data, err = io.ReadAll(io.LimitReader(in, RecipeLimit+1))
		} else {
			data, err = os.ReadFile(*recipe)
		}
		if err != nil {
			return printError(out, errw, Errorf(CodeInvalid, "cannot read the recipe: %v", err))
		}
		if len(data) > RecipeLimit {
			return printError(out, errw, Errorf(CodeInvalid, "the recipe is longer than the %d bytes an image carries", RecipeLimit))
		}
		if len(data) > 0 {
			req.Recipe = base64.StdEncoding.EncodeToString(data)
		}
	}
	res, err := c.Seal(ctx, req, func(line string) {
		printJSON(out, map[string]string{"line": line})
	})
	if err != nil {
		return printError(out, errw, err)
	}
	return printJSON(out, map[string]any{"seal": res})
}
