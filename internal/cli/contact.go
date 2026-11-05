package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/hashicorp/nomad/api"
)

// A box is reached through Nomad: the API for its state, and `nomad alloc exec`
// for everything in the guest. Either one can be lost for a moment -- the client
// node restarts for a driver update, the server's connection to it drops, the
// guest's boxd is restarted -- and none of that is a turn that ended.
//
// A command that follows work in a box therefore keeps trying while the contact
// is lost, and says so once on stderr when it goes and once when it comes back.
// Only a box that is really gone ends the wait: stopped, failed, removed.
const (
	// contactRetryFirst is the first pause after contact is lost.
	contactRetryFirst = 500 * time.Millisecond
	// contactRetryMax caps the pause between the attempts.
	contactRetryMax = 5 * time.Second
)

// lostContactMarkers are what a connection that went away reads like, from
// Nomad's API, from its exec stream and from boxd's socket. The answer of a
// guest that is there -- busy, not_found, executor_error -- is not one of them:
// that is the box talking.
var lostContactMarkers = []string{
	"eof",
	"connection refused",
	"connection reset",
	"connection closed",
	"broken pipe",
	"no route to host",
	"network is unreachable",
	"no path to node",
	"no known peers",
	"client connection lost",
	"use of closed network connection",
	"i/o timeout",
	"timeout awaiting response headers",
	"tls handshake timeout",
	"websocket: close",
	"server is unavailable",
	"service unavailable",
	"bad gateway",
	"gateway timeout",
	"internal server error",
	"rpc error: code = unavailable",
	"rpc error: code = deadlineexceeded",
	"allocation is not running",
	"alloc is not running",
	"failed to connect to the node",
	"node is down",
	"connection to the node was lost",
	"could not reach the node",
}

// attemptBound is a guest call that did not answer in its time. Nomad's exec can
// leave a call waiting while its server looks for a node that is restarting, and
// ending the call's context does not end it: a call that overruns its bound is a
// lost connection like any other.
type attemptBound struct {
	waited time.Duration
	what   string
}

func (e *attemptBound) Error() string {
	return fmt.Sprintf("%s did not answer within %s", e.what, e.waited.Round(time.Second))
}

// boundedCall runs one guest call and gives up on it after bound, returning an
// attemptBound. bound <= 0 means "wait for as long as the call takes".
//
// The call runs in a goroutine of its own because the transport can ignore the
// end of its context; the goroutine ends when the transport finally notices, and
// the command is about to return anyway.
func boundedCall[T any](bound time.Duration, what string, call func() (T, error)) (T, error) {
	if bound <= 0 {
		return call()
	}
	type result struct {
		v   T
		err error
	}
	ch := make(chan result, 1)
	go func() {
		v, err := call()
		ch <- result{v: v, err: err}
	}()
	timer := time.NewTimer(bound)
	defer timer.Stop()
	select {
	case r := <-ch:
		return r.v, r.err
	case <-timer.C:
		var zero T
		return zero, &attemptBound{waited: bound, what: what}
	}
}

// genuineGuestError reports whether err is an answer from boxd: the box is there
// and it said no. Such an error is never retried, whatever its words are.
func genuineGuestError(err error) bool {
	var bce *BoxctlError
	if errors.As(err, &bce) {
		return true
	}
	var e *Error
	return errors.As(err, &e) && e.Err == nil && e.Msg != ""
}

// isBoxStateError reports whether err says the box itself is not running.
func isBoxStateError(err error) bool {
	var se *boxStateError
	return errors.As(err, &se)
}

// lostContact reports whether err is a loss of contact with the box or with
// Nomad, and not an answer from either.
func lostContact(err error) bool {
	if err == nil {
		return false
	}
	var overrun *attemptBound
	if errors.As(err, &overrun) {
		return true
	}
	// A box that is not running is an answer, even though its sentence may talk
	// about a node: only the state of the box ends a wait.
	if isBoxStateError(err) {
		return false
	}
	// A box that is starting, or unknown because its node is away, is not an
	// answer either: it may be back in a moment.
	var pending *notRunningYet
	if errors.As(err, &pending) {
		return true
	}
	if genuineGuestError(err) {
		return false
	}
	// Nomad answers a 5xx for a client it cannot reach; any other status is a
	// reply about the request.
	var unexpected *api.UnexpectedResponseError
	if errors.As(err, &unexpected) && unexpected.HasStatusCode() {
		return unexpected.StatusCode() >= 500
	}
	text := strings.ToLower(err.Error())
	for _, marker := range lostContactMarkers {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}

// contactOfBox is the contact state of one command with one box, shared by the
// streams a command follows (the two task logs, for one).
type contactOfBox struct {
	a   *App
	box string

	mu    sync.Mutex
	down  map[string]bool
	since time.Time
}

// newContact returns the contact tracker of one command.
func (a *App) newContact(box string) *contactOfBox {
	return &contactOfBox{a: a, box: box, down: map[string]bool{}}
}

// lost records that stream lost its contact with the box and prints the line
// once, however many streams report it. It returns how long contact has been
// lost, and whether this stream was the first to notice.
func (c *contactOfBox) lost(stream string, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.down[stream] {
		return
	}
	c.down[stream] = true
	if len(c.down) > 1 {
		return
	}
	c.since = c.a.now()
	// One line, the same words every time: the cause is in what the attempt
	// reports when the box does not come back, and a coordinator greps this one.
	_ = err
	c.a.note("lost contact with the box; retrying")
}

// back records that a stream is in contact again and prints the line once, when
// the last stream that was lost is back.
func (c *contactOfBox) back(stream string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.down[stream] {
		return
	}
	delete(c.down, stream)
	if len(c.down) > 0 {
		return
	}
	lost := c.a.now().Sub(c.since)
	c.a.note("back in contact after %s", loseSentence(lost))
}

// lostFor is how long contact has been lost.
func (c *contactOfBox) lostFor() time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.since.IsZero() {
		return 0
	}
	d := c.a.now().Sub(c.since)
	if d < 0 {
		return 0
	}
	return d
}

// loseSentence renders how long contact was lost, in the words a person reads.
func loseSentence(d time.Duration) string {
	if d < time.Second {
		return "a moment"
	}
	d = d.Round(time.Second)
	if d < time.Minute {
		return d.String()
	}
	return d.String()
}

// contactBackoff is how long to wait before the next attempt after n failures.
func contactBackoff(n int) time.Duration {
	d := contactRetryFirst
	for i := 1; i < n && d < contactRetryMax; i++ {
		d *= 2
	}
	if d > contactRetryMax {
		d = contactRetryMax
	}
	return d
}

// boxGone reports whether the box really ended, and the error that says so.
//
// It is what a command asks while it has lost contact: a box that was stopped,
// that failed or that was removed is not a lost connection, and its own sentence
// is the answer. An error from Nomad here is not an answer at all -- the command
// keeps trying.
func (a *App) boxGone(ctx context.Context, id string) error {
	if ctx.Err() != nil {
		return nil
	}
	callCtx, cancel := context.WithTimeout(ctx, guestCallTimeout)
	defer cancel()
	box, _, err := a.findBox(callCtx, id)
	if err != nil {
		return nil
	}
	switch box.State {
	case "running":
		return nil
	case "stopped", "failed", "orphan", "gone":
		return boxNotRunning(box)
	}
	return nil
}

// waitForContact waits before the next attempt while contact is lost, and
// returns the box's own error when the box is really gone.
func (c *contactOfBox) waitForContact(ctx context.Context, id string, attempt int) error {
	if err := c.a.sleep(ctx, contactBackoff(attempt)); err != nil {
		return wrapf(err, "stopped waiting for the box %s", id)
	}
	if err := c.a.boxGone(ctx, id); err != nil {
		return err
	}
	return nil
}

// unknownAfter is the sentence of a command that ran out of its budget while
// contact was lost: the work may still be going on, and saying "failed" would
// make an agent restart work that is running.
func unknownAfter(what, box string, lostFor time.Duration, hint string) error {
	msg := fmt.Sprintf("lost contact with the box %s for %s: %s is unknown and it may still be running, "+
		"so this is neither a failure nor a timeout%s", box, loseSentence(lostFor), what, hint)
	return unknownf("%s", msg)
}
