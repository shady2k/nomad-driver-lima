package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

// sessionNewOutput is the JSON document of `box session new`.
type sessionNewOutput struct {
	Session string   `json:"session"`
	Box     string   `json:"box"`
	Details *Session `json:"details,omitempty"`
}

// sessionListOutput is the JSON document of `box session ls`.
type sessionListOutput struct {
	Box      string    `json:"box"`
	Sessions []Session `json:"sessions"`
}

// cmdSession dispatches the session subcommands.
func (a *App) cmdSession(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return usagef("box session needs a subcommand: new, ls or close")
	}
	switch args[0] {
	case "new":
		return a.cmdSessionNew(ctx, args[1:])
	case "ls", "list":
		return a.cmdSessionList(ctx, args[1:])
	case "close":
		return a.cmdSessionClose(ctx, args[1:])
	case "help", "-h", "--help":
		fmt.Fprintf(a.ErrOut, "usage: box session new <id> --executor NAME --model PROVIDER/MODEL [--title T] [--cwd DIR]\n"+
			"       box session ls <id>\n"+
			"       box session close <id> <session>\n"+
			"`box session new --help` lists its flags; `box executors show` the executors and their models.\n")
		return nil
	default:
		return usagef("unknown session subcommand %q: use new, ls or close", args[0])
	}
}

// cmdSessionNew starts an agent session in a box.
func (a *App) cmdSessionNew(ctx context.Context, args []string) error {
	fs := newFlagSet("session new")
	executor := fs.String("executor", "", "agent executor: prime, opencode or claude")
	model := fs.String("model", "", "model as <provider>/<model>")
	cwd := fs.String("cwd", "", "working directory for the agent")
	title := fs.String("title", "", "a short title for the session")
	if err := parseFlags(fs, args, 1); err != nil {
		return err
	}
	id := fs.Arg(0)
	if strings.TrimSpace(*executor) == "" {
		return usagef("box session new %s: --executor is required (prime, opencode or claude)", id)
	}
	if strings.TrimSpace(*model) == "" {
		return usagef("box session new %s: --model is required, as <provider>/<model> (for example --model myprovider/mymodel)", id)
	}
	client, _, err := a.boxctlFor(ctx, id)
	if err != nil {
		return err
	}
	sessionID, err := client.SessionNew(ctx, SessionNewOptions{
		Executor: *executor,
		Model:    *model,
		Cwd:      *cwd,
		Title:    *title,
	})
	if err != nil {
		return wrapf(err, "the guest could not start a %s session", *executor)
	}
	out := sessionNewOutput{Session: sessionID, Box: id}
	if details, err := client.SessionGet(ctx, sessionID); err == nil {
		out.Details = details
	}
	if a.JSON {
		return a.printJSON(out)
	}
	a.note("started session %s in box %s (%s %s)", sessionID, id, *executor, *model)
	a.note("next: box send %s %s \"<prompt>\"", id, sessionID)
	return nil
}

// cmdSessionList lists the sessions of a box.
func (a *App) cmdSessionList(ctx context.Context, args []string) error {
	fs := newFlagSet("session ls")
	if err := parseFlags(fs, args, 1); err != nil {
		return err
	}
	id := fs.Arg(0)
	client, _, err := a.boxctlFor(ctx, id)
	if err != nil {
		return err
	}
	sessions, err := client.SessionList(ctx)
	if err != nil {
		return wrapf(err, "the guest could not list the sessions")
	}
	if a.JSON {
		return a.printJSON(sessionListOutput{Box: id, Sessions: sessions})
	}
	if len(sessions) == 0 {
		a.note("box %s has no sessions; start one with `box session new %s --executor prime --model <provider>/<model>`", id, id)
		return nil
	}
	rows := make([][]string, 0, len(sessions))
	for _, session := range sessions {
		rows = append(rows, []string{
			session.ID,
			dash(session.Executor),
			dash(session.Model),
			session.State,
			fmt.Sprintf("%d", session.LastTurn),
			dash(session.Title),
			dash(session.Cwd),
		})
	}
	a.printTable([]string{"SESSION", "EXECUTOR", "MODEL", "STATE", "TURNS", "TITLE", "CWD"}, rows)
	return nil
}

// cmdSessionClose closes a session, keeping its history.
func (a *App) cmdSessionClose(ctx context.Context, args []string) error {
	fs := newFlagSet("session close")
	if err := parseFlags(fs, args, 2); err != nil {
		return err
	}
	id, sessionID := fs.Arg(0), fs.Arg(1)
	client, _, err := a.boxctlFor(ctx, id)
	if err != nil {
		return err
	}
	if err := client.SessionClose(ctx, sessionID); err != nil {
		return wrapf(err, "the guest could not close session %s", sessionID)
	}
	if a.JSON {
		return a.printJSON(map[string]string{"session": sessionID, "state": "closed", "box": id})
	}
	a.note("closed session %s in box %s; its history is kept", sessionID, id)
	return nil
}

// cmdSend sends a prompt to a session.
func (a *App) cmdSend(ctx context.Context, args []string) error {
	fs := newFlagSet("send")
	file := fs.String("file", "", "read the prompt from this local file")
	if err := parseFlagsMin(fs, args, 2); err != nil {
		return err
	}
	id, sessionID := fs.Arg(0), fs.Arg(1)
	words := fs.Args()[2:]
	prompt := strings.Join(words, " ")
	if *file != "" && strings.TrimSpace(prompt) != "" {
		return usagef("box send %s %s: give either --file or the prompt text, not both", id, sessionID)
	}
	if *file != "" {
		data, err := readFile(*file)
		if err != nil {
			return err
		}
		prompt = data
	}
	if strings.TrimSpace(prompt) == "" {
		data, err := readAllStdin(a)
		if err != nil {
			return err
		}
		prompt = data
	}
	if strings.TrimSpace(prompt) == "" {
		return usagef("box send %s %s: the prompt is empty; pass it as arguments, with --file, or on stdin", id, sessionID)
	}
	client, _, err := a.boxctlFor(ctx, id)
	if err != nil {
		return err
	}
	turn, err := client.Send(ctx, sessionID, prompt)
	if err != nil {
		var bce *BoxctlError
		if errors.As(err, &bce) && bce.Code == "busy" {
			return errorf("session %s is still working on a turn; `box wait %s %s` waits for it, `box cancel %s %s` stops it",
				sessionID, id, sessionID, id, sessionID)
		}
		return wrapf(err, "the guest could not send the prompt to session %s", sessionID)
	}
	if a.JSON {
		return a.printJSON(map[string]any{"box": id, "session": sessionID, "turn": turn})
	}
	a.note("sent to session %s in box %s: turn %d started", sessionID, id, turn)
	a.note("next: box wait %s %s --timeout 15m", id, sessionID)
	return nil
}

// cmdWait waits for the running turn of a session.
func (a *App) cmdWait(ctx context.Context, args []string) error {
	fs := newFlagSet("wait")
	timeout := fs.Duration("timeout", defaultWaitTimeout, "how long to wait; the turn keeps running when this expires")
	turnNumber := fs.Int("turn", 0, "wait for this turn number instead of the running one")
	if err := parseFlags(fs, args, 2); err != nil {
		return err
	}
	id, sessionID := fs.Arg(0), fs.Arg(1)
	client, _, err := a.boxctlFor(ctx, id)
	if err != nil {
		return err
	}
	waitCtx := ctx
	var cancel context.CancelFunc
	// Ask boxctl for one second less than our own budget, so its timeout
	// arrives first and we can report the turn as still running.
	seconds := int(timeout.Seconds())
	if seconds > 1 {
		seconds--
	}
	if *timeout > 0 {
		waitCtx, cancel = context.WithTimeout(ctx, *timeout+15*time.Second)
		defer cancel()
	}
	turn, err := client.Wait(waitCtx, sessionID, WaitOptions{Turn: *turnNumber, Timeout: seconds})
	if err != nil {
		if errors.Is(waitCtx.Err(), context.DeadlineExceeded) {
			return timeoutf("the turn of session %s is still running after %s; `box events %s %s -f` shows what it is doing",
				sessionID, *timeout, id, sessionID)
		}
		return wrapf(err, "the guest could not wait for session %s", sessionID)
	}
	if turn.State == "running" {
		if a.JSON {
			if err := a.printJSON(turn); err != nil {
				return err
			}
		}
		return timeoutf("the turn %d of session %s is still running after %s; `box events %s %s -f` shows what it is doing",
			turn.Turn, sessionID, *timeout, id, sessionID)
	}
	if a.JSON {
		if err := a.printJSON(turn); err != nil {
			return err
		}
	} else {
		if turn.Text != "" {
			fmt.Fprintln(a.Out, turn.Text)
		}
		a.note("session %s turn %d finished: %s%s", sessionID, turn.Turn, dash(turn.State), turnSuffix(turn))
		if turn.State == "done" && strings.TrimSpace(turn.Text) == "" {
			// An agent may end a turn on a tool call with nothing to say; an
			// empty answer is that, not a lost one.
			a.note("the agent ended the turn without a final message; `box events %s %s` shows what it did", id, sessionID)
		}
	}
	// A script tells a failed turn from a good one by the exit code alone.
	if turn.State != "done" {
		return errorf("the turn %d of session %s ended %s%s; `box events %s %s` shows what happened",
			turn.Turn, sessionID, dash(turn.State), turnSuffix(turn), id, sessionID)
	}
	return nil
}

func turnSuffix(turn *Turn) string {
	var parts []string
	if turn.StopReason != "" {
		parts = append(parts, "stop reason "+turn.StopReason)
	}
	if turn.Error != "" {
		parts = append(parts, "error: "+oneLine(turn.Error, 200))
	}
	if len(parts) == 0 {
		return ""
	}
	return " (" + strings.Join(parts, ", ") + ")"
}

// cmdEvents streams the events of a session.
func (a *App) cmdEvents(ctx context.Context, args []string) error {
	fs := newFlagSet("events")
	since := fs.Int64("since", 0, "resume from this sequence number")
	limit := fs.Int("limit", 200, "how many events to ask for per call")
	follow := fs.Bool("f", false, "keep following the stream")
	if err := parseFlags(fs, args, 2); err != nil {
		return err
	}
	id, sessionID := fs.Arg(0), fs.Arg(1)
	client, _, err := a.boxctlFor(ctx, id)
	if err != nil {
		return err
	}
	cursor := *since
	for {
		events, next, err := client.Events(ctx, sessionID, EventsOptions{Since: cursor, Limit: *limit})
		if err != nil {
			return wrapf(err, "the guest could not read the events of session %s", sessionID)
		}
		for _, event := range events {
			if err := a.printEvent(event); err != nil {
				return err
			}
		}
		if next > cursor {
			cursor = next
		}
		if !*follow {
			return nil
		}
		if err := a.sleep(ctx, 2*time.Second); err != nil {
			return nil
		}
	}
}

// printEvent writes one event: a JSON object per line with --json, otherwise a
// readable line.
func (a *App) printEvent(event Event) error {
	if a.JSON {
		data, err := json.Marshal(event)
		if err != nil {
			return wrapf(err, "cannot write event %d", event.Seq)
		}
		fmt.Fprintln(a.Out, string(data))
		return nil
	}
	fmt.Fprintf(a.Out, "%6d  %s  %-12s %s\n", event.Seq, dash(event.TS), event.Kind, event.Summary)
	return nil
}

// cmdCancel cancels the running turn of a session.
func (a *App) cmdCancel(ctx context.Context, args []string) error {
	fs := newFlagSet("cancel")
	if err := parseFlags(fs, args, 2); err != nil {
		return err
	}
	id, sessionID := fs.Arg(0), fs.Arg(1)
	client, _, err := a.boxctlFor(ctx, id)
	if err != nil {
		return err
	}
	turn, err := client.Cancel(ctx, sessionID)
	if err != nil {
		return wrapf(err, "the guest could not cancel session %s", sessionID)
	}
	if a.JSON {
		return a.printJSON(turn)
	}
	a.note("cancelled session %s in box %s (turn %d is %s)", sessionID, id, turn.Turn, dash(turn.State))
	return nil
}

// readFile reads a local file as text.
func readFile(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", wrapf(err, "cannot read %s", path)
	}
	return string(data), nil
}

// readAllStdin reads the whole of stdin, unless it is a terminal.
func readAllStdin(a *App) (string, error) {
	if a.In == nil {
		return "", nil
	}
	if a.Terminal != nil && a.Terminal.IsInteractive() {
		return "", usagef("no prompt text was given: pass it as arguments or with --file (stdin is the terminal)")
	}
	data, err := io.ReadAll(a.In)
	if err != nil {
		return "", wrapf(err, "cannot read the prompt from stdin")
	}
	return string(data), nil
}
