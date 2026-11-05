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
	// GuestError explains why the guest did not answer. It is set only when the
	// guest ran out of its call bound, and the command then exits 4.
	GuestError string `json:"guest_error,omitempty"`
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
	// Another session already writing in this directory is not a refusal -- two
	// sessions in one box is normal -- but it is what a second writer in the same
	// working copy looks like, so the agent is told before it sends a prompt.
	if out.Details != nil {
		a.warnSameDirectory(ctx, client, sessionID, out.Details.Cwd)
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
	var sessions []Session
	err = a.guestCall(ctx, "`boxctl session ls`", func(callCtx context.Context) error {
		got, err := client.SessionList(callCtx)
		if err != nil {
			return err
		}
		sessions = got
		return nil
	})
	if err != nil {
		if isGuestSilent(err) {
			return a.guestSilentFailure(id, err, sessionListOutput{Box: id, GuestError: err.Error()})
		}
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
			session.stateLabel(),
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
	if cwd, ok := a.sessionCwd(ctx, client, sessionID); ok {
		a.warnSameDirectory(ctx, client, sessionID, cwd)
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

// sessionCwd returns the working directory of one session of the box.
func (a *App) sessionCwd(ctx context.Context, client *Boxctl, sessionID string) (string, bool) {
	var sessions []Session
	err := a.guestCall(ctx, "`boxctl session ls`", func(callCtx context.Context) error {
		got, err := client.SessionList(callCtx)
		if err != nil {
			return err
		}
		sessions = got
		return nil
	})
	if err != nil {
		return "", false
	}
	for _, session := range sessions {
		if session.ID == sessionID {
			return session.Cwd, session.Cwd != ""
		}
	}
	return "", false
}

// warnSameDirectory tells the caller when another session of this box is working
// in the same directory: a running turn, or an executor that is still sending
// updates after a turn that ended in an error. Two sessions in one working copy
// is what a second writer looked like.
//
// It is a warning on stderr and never a refusal: the exit code does not change,
// and a boxd that does not answer this question stays quiet.
func (a *App) warnSameDirectory(ctx context.Context, client *Boxctl, sessionID, cwd string) {
	if cwd == "" {
		return
	}
	var sessions []Session
	err := a.guestCall(ctx, "`boxctl session ls`", func(callCtx context.Context) error {
		got, err := client.SessionList(callCtx)
		if err != nil {
			return err
		}
		sessions = got
		return nil
	})
	if err != nil {
		return
	}
	for _, other := range sessions {
		if other.ID == sessionID || other.Cwd != cwd {
			continue
		}
		if other.State != "running" && !other.activeOutsideTurn() {
			continue
		}
		a.note("session %s is working in the same directory", other.ID)
	}
}

// cmdWait waits for the running turn of a session.
//
// A lost connection to the box or to Nomad is not the end of the turn: the wait
// keeps trying, with a pause between the attempts, to the end of --timeout, and
// says on stderr when contact went and when it came back. It reconnects to the
// same turn, so a turn that finished while the box was unreachable is reported
// with its real result. Only a box that is really gone ends the wait, and a wait
// that runs out of contact reports the new exit code: the turn's state is
// unknown and it may still run.
func (a *App) cmdWait(ctx context.Context, args []string) error {
	fs := newFlagSet("wait")
	timeout := fs.Duration("timeout", defaultWaitTimeout, "how long to wait; the turn keeps running when this expires")
	turnNumber := fs.Int("turn", 0, "wait for this turn number instead of the running one")
	if err := parseFlags(fs, args, 2); err != nil {
		return err
	}
	id, sessionID := fs.Arg(0), fs.Arg(1)
	turn := *turnNumber
	contact := a.newContact(id)
	attempts := 0
	// The budget is measured from the first loss of contact: the wait a healthy
	// box gives is exactly the one --timeout asks for.
	var deadline time.Time
	for {
		// Ask boxctl for one second less than our own budget, so its timeout
		// arrives first and we can report the turn as still running.
		seconds := 0
		budget := time.Duration(0)
		if *timeout > 0 {
			seconds = int((*timeout).Seconds()) - 1
			budget = *timeout + 15*time.Second
			if attempts > 0 {
				remaining := deadline.Sub(a.now())
				seconds = int(remaining.Seconds()) - 1
				budget = remaining + 15*time.Second
			}
		}
		// The attempt is given up on a little after boxctl's own timeout: a
		// healthy call answers by then, and a call that does not is a connection
		// that is not there, however the transport feels about it.
		attemptBound := time.Duration(0)
		if *timeout > 0 {
			attemptBound = time.Duration(seconds)*time.Second + 5*time.Second
			if attemptBound < 2*time.Second {
				attemptBound = 2 * time.Second
			}
		}
		client, _, err := a.boxctlRunning(ctx, id)
		if err == nil {
			callCtx := ctx
			cancel := func() {}
			if *timeout > 0 {
				callCtx, cancel = context.WithTimeout(ctx, budget)
			}
			var got *Turn
			got, err = boundedCall(attemptBound, "`boxctl wait`", func() (*Turn, error) {
				return client.Wait(callCtx, sessionID, WaitOptions{Turn: turn, Timeout: seconds})
			})
			cancel()
			if err == nil {
				contact.back("wait")
				return a.reportTurn(got, id, sessionID, *timeout)
			}
		}
		if !lostContact(err) {
			if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
				return timeoutf("the turn of session %s is still running after %s; `box events %s %s -f` shows what it is doing",
					sessionID, *timeout, id, sessionID)
			}
			if isBoxStateError(err) {
				return err
			}
			return wrapKeep(err, "the guest could not wait for session %s", sessionID)
		}
		// Contact is lost: this is not a turn that ended.
		contact.lost("wait", err)
		if deadline.IsZero() {
			deadline = a.now().Add(*timeout)
		}
		attempts++
		if *timeout > 0 && !a.now().Before(deadline) {
			return unknownAfter(fmt.Sprintf("the state of the turn of session %s", sessionID), id, contact.lostFor(),
				fmt.Sprintf("; `box events %s %s --since <seq>` shows what it did once the box answers again", id, sessionID))
		}
		if err := contact.waitForContact(ctx, id, attempts); err != nil {
			return err
		}
		// Back? One cheap guest call proves it, says so on stderr, and gives the
		// turn to keep waiting for: the retry is the same turn, not whatever
		// turn comes next.
		if last, err := a.lastTurn(ctx, client, sessionID); err == nil {
			contact.back("wait")
			if turn == 0 && last > 0 {
				turn = last
			}
		}
	}
}

// lastTurn returns the number of the last turn of a session, so a wait that lost
// its connection resumes on the turn it was waiting for.
func (a *App) lastTurn(ctx context.Context, client *Boxctl, sessionID string) (int, error) {
	if client == nil {
		return 0, errorf("the box is not reachable")
	}
	var session *Session
	err := a.guestCall(ctx, "`boxctl session get`", func(callCtx context.Context) error {
		got, err := client.SessionGet(callCtx, sessionID)
		if err != nil {
			return err
		}
		session = got
		return nil
	})
	if err != nil {
		return 0, err
	}
	if session == nil {
		return 0, nil
	}
	return session.LastTurn, nil
}

// reportTurn prints the result of a turn the wait saw finish.
func (a *App) reportTurn(turn *Turn, id, sessionID string, timeout time.Duration) error {
	if turn.State == "running" {
		if a.JSON {
			if err := a.printJSON(turn); err != nil {
				return err
			}
		}
		return timeoutf("the turn %d of session %s is still running after %s; `box events %s %s -f` shows what it is doing",
			turn.Turn, sessionID, timeout, id, sessionID)
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
	// What the turn left running is said after the turn's own words, and it
	// changes neither the turn's state nor the exit code below: when the turn
	// ended is the executor's decision.
	a.noteStillRunning(turn.StillRunning, id, sessionID)
	// A script tells a failed turn from a good one by the exit code alone.
	if turn.State != "done" {
		return errorf("the turn %d of session %s ended %s%s; `box events %s %s` shows what happened",
			turn.Turn, sessionID, dash(turn.State), turnSuffix(turn), id, sessionID)
	}
	return nil
}

// noteStillRunning says what a finished turn left running in the box, how to
// look at it and how to go on.
//
// A done turn with processes still running is not finished work: the executor
// ended its turn, and the build or the server it started did not. The wording
// is boxd's -- these are processes it saw, not a verdict on the work -- and
// the exit code stays the turn's.
func (a *App) noteStillRunning(still *StillRunning, box, sessionID string) {
	if still == nil || still.Count() == 0 {
		return
	}
	a.note("the turn ended, but these processes it may have started are still running in the box:")
	for _, line := range still.Lines() {
		a.note("  %s", line)
	}
	a.note("`box top %s` shows what the guest is doing and `box run %s -- ps -ef` lists the processes;", box, box)
	a.note("when they are done, `box send %s %s` starts the next turn, or ask the session to wait for them.",
		box, sessionID)
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
//
// The cursor is the sequence number of the last event printed, so a stream that
// loses its connection to the box resumes where it was: nothing is printed twice
// and nothing is skipped. A lost connection is not the end of a feed; only a box
// that is really gone ends it.
func (a *App) cmdEvents(ctx context.Context, args []string) error {
	fs := newFlagSet("events")
	since := fs.Int64("since", 0, "resume from this sequence number")
	limit := fs.Int("limit", 200, "how many events to ask for per call")
	tail := fs.Int("tail", 0, "print the last N events only")
	follow := fs.Bool("f", false, "keep following the stream")
	if err := parseFlags(fs, args, 2); err != nil {
		return err
	}
	id, sessionID := fs.Arg(0), fs.Arg(1)
	if *tail < 0 {
		return usagef("box events %s %s: --tail %d is not a number of events", id, sessionID, *tail)
	}
	if *tail > 0 && *follow {
		return usagef("box events %s %s: --tail N reads what a session already did; --f follows what it does next", id, sessionID)
	}
	client, _, err := a.boxctlRunning(ctx, id)
	if err != nil {
		if !lostContact(err) {
			return err
		}
		// The box is not reachable yet: keep trying for as long as the caller
		// lets us, the same way the follow loop does.
		contact := a.newContact(id)
		contact.lost("events", err)
		attempts := 0
		for {
			attempts++
			if err := contact.waitForContact(ctx, id, attempts); err != nil {
				return err
			}
			fresh, _, err := a.boxctlRunning(ctx, id)
			if err == nil {
				client = fresh
				contact.back("events")
				break
			}
			if !lostContact(err) {
				return err
			}
		}
	}
	if *tail > 0 {
		return a.tailEvents(ctx, client, id, sessionID, *tail)
	}
	cursor := *since
	contact := a.newContact(id)
	attempts := 0
	for {
		page, err := boundedCall(a.guestTimeout(), "`boxctl events`", func() (EventPage, error) {
			return client.Events(ctx, sessionID, EventsOptions{Since: cursor, Limit: *limit})
		})
		if err != nil {
			if !lostContact(err) {
				return wrapf(err, "the guest could not read the events of session %s", sessionID)
			}
			contact.lost("events", err)
			attempts++
			if err := contact.waitForContact(ctx, id, attempts); err != nil {
				return err
			}
			// The box was away: ask Nomad for the guest again, in case the
			// allocation came back under the same box.
			if fresh, _, err := a.boxctlRunning(ctx, id); err == nil {
				client = fresh
			}
			continue
		}
		contact.back("events")
		for _, event := range page.Events {
			if err := a.printEvent(event); err != nil {
				return err
			}
		}
		if page.Next > cursor {
			cursor = page.Next
		}
		if !*follow {
			// A page is not the feed. Saying so is what keeps a limit from
			// reading as a hang: the next call, with --since, is the rest.
			a.noteMoreEvents(page, cursor)
			return nil
		}
		if err := a.sleep(ctx, 2*time.Second); err != nil {
			return nil
		}
	}
}

// tailEvents prints the last n events of a session: the news, not the whole feed.
//
// The feed counts from 1 and has no gaps, and a page says how many events there
// are after the cursor, so the last n are the ones from total-n+1. One cheap
// first call is what makes that a two-call answer instead of reading the feed.
func (a *App) tailEvents(ctx context.Context, client *Boxctl, id, sessionID string, n int) error {
	fetch := func(opts EventsOptions) (EventPage, error) {
		return boundedCall(a.guestTimeout(), "`boxctl events`", func() (EventPage, error) {
			return client.Events(ctx, sessionID, opts)
		})
	}
	head, err := fetch(EventsOptions{Limit: 1})
	if err != nil {
		if !lostContact(err) {
			return wrapf(err, "the guest could not read the events of session %s", sessionID)
		}
		// The count is not known while the box is away; asking again from the
		// start is the same answer once it is back.
		contact := a.newContact(id)
		contact.lost("events", err)
		attempts := 0
		for {
			attempts++
			if err := contact.waitForContact(ctx, id, attempts); err != nil {
				return err
			}
			if fresh, _, err := a.boxctlRunning(ctx, id); err == nil {
				client = fresh
			}
			head, err = fetch(EventsOptions{Limit: 1})
			if err == nil {
				contact.back("events")
				break
			}
			if !lostContact(err) {
				return wrapf(err, "the guest could not read the events of session %s", sessionID)
			}
		}
	}
	since := int64(head.Total) - int64(n) + 1
	if since < 1 {
		since = 1
	}
	page, err := fetch(EventsOptions{Since: since, Limit: n})
	if err != nil {
		return wrapf(err, "the guest could not read the events of session %s", sessionID)
	}
	for _, event := range page.Events {
		if err := a.printEvent(event); err != nil {
			return err
		}
	}
	return nil
}

// noteMoreEvents says that the events printed were a page of the feed and not
// all of it, with the cursor that continues them.
//
// An agent that reads 200 events and stops takes the end of the page for the end
// of the feed and waits for events that are already there. With --json the same
// fact is the last document on stdout, one line after the events; a reader that
// lives on the text reads it on stderr.
func (a *App) noteMoreEvents(page EventPage, cursor int64) {
	if !page.More {
		return
	}
	if a.JSON {
		// One line, like the events before it: a document per line is what a
		// reader of a JSON stream expects.
		data, err := json.Marshal(map[string]any{"next_since": cursor, "more": true})
		if err != nil {
			return
		}
		fmt.Fprintln(a.Out, string(data))
		return
	}
	a.note("%d of %d events, more with --since %d", len(page.Events), page.Total, cursor)
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
