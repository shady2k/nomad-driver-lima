package boxd

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// feed writes n events to a session's feed, the way boxd does while it works.
func feed(t *testing.T, m *Manager, id string, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		ev := Event{Seq: int64(i + 1), TS: time.Date(2026, 11, 5, 12, 0, i, 0, time.UTC),
			Session: id, Turn: 1, Kind: KindMessage, Summary: "line " + string(rune('a'+i))}
		if err := m.store.AppendEvent(id, ev); err != nil {
			t.Fatalf("append event %d: %v", i+1, err)
		}
	}
}

// TestEventsPageSaysHowManyThereAre: reading an event feed returns a page, and a
// page says how much of the feed it is -- the limit is not the end of the events,
// and an agent that took it for one waited for events that were already there.
func TestEventsPageSaysHowManyThereAre(t *testing.T) {
	m := newManager(t, testConfig(t))
	s := mustSession(t, m, "prime", "p/m")
	feed(t, m, s.ID, 5)

	evs, next, total, err := m.EventsPage(s.ID, 0, 2)
	if err != nil {
		t.Fatalf("EventsPage: %v", err)
	}
	if len(evs) != 2 || next != 3 || total != 5 {
		t.Fatalf("%d event(s), next %d, total %d, want 2, 3 and 5", len(evs), next, total)
	}
	// The whole feed in one page: nothing was left out.
	evs, next, total, err = m.EventsPage(s.ID, 0, 100)
	if err != nil {
		t.Fatalf("EventsPage: %v", err)
	}
	if len(evs) != 5 || next != 6 || total != 5 {
		t.Fatalf("%d event(s), next %d, total %d, want all 5, 6 and 5", len(evs), next, total)
	}
	// A cursor is where the next read starts and reads that event again: the
	// page boundary of a feed read by pages loses nothing.
	evs, next, total, err = m.EventsPage(s.ID, 3, 10)
	if err != nil {
		t.Fatalf("EventsPage: %v", err)
	}
	if len(evs) != 3 || evs[0].Seq != 3 || next != 6 || total != 3 {
		t.Fatalf("%d event(s) from seq %d, next %d, total %d, want the three from 3", len(evs), evs[0].Seq, next, total)
	}
	// Events keeps its own signature and its cursor.
	evs, next, err = m.Events(s.ID, 3, 10)
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	if len(evs) != 3 || next != 6 {
		t.Fatalf("%d event(s), next %d, want 3 and 6", len(evs), next)
	}
	// A session with no feed yet is an empty page and no error.
	evs, next, total, err = m.EventsPage(s.ID, 6, 10)
	if err != nil || len(evs) != 0 || next != 6 || total != 0 {
		t.Fatalf("%d event(s), next %d, total %d, err %v", len(evs), next, total, err)
	}
}

// TestTheEventAnswerCarriesMore: what boxd answers says that it is a page, so
// boxctl and the CLI can say it in turn.
func TestTheEventAnswerCarriesMore(t *testing.T) {
	cfg := testConfig(t)
	d, err := NewDaemon(cfg)
	if err != nil {
		t.Fatalf("NewDaemon: %v", err)
	}
	t.Cleanup(d.Manager().Shutdown)
	s := mustSession(t, d.Manager(), "prime", "p/m")
	feed(t, d.Manager(), s.ID, 3)

	rec := httptest.NewRecorder()
	d.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/session/"+s.ID+"/events?limit=2", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var doc struct {
		Events []Event `json:"events"`
		Next   int64   `json:"next"`
		Total  int     `json:"total"`
		More   bool    `json:"more"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(doc.Events) != 2 || doc.Next != 3 || doc.Total != 3 || !doc.More {
		t.Fatalf("%d event(s), next %d, total %d, more %v, want 2, 3, 3 and true",
			len(doc.Events), doc.Next, doc.Total, doc.More)
	}

	rec = httptest.NewRecorder()
	d.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/session/"+s.ID+"/events", nil))
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(doc.Events) != 3 || doc.Total != 3 || doc.More {
		t.Fatalf("%d event(s), total %d, more %v, want the whole feed", len(doc.Events), doc.Total, doc.More)
	}
}

// TestBoxctlEventsSaysWhenItWasCut: boxctl's own answer says that it printed a
// page, on stderr, with the cursor that continues it -- a limit read as the end of
// the feed is what this is for.
func TestBoxctlEventsSaysWhenItWasCut(t *testing.T) {
	cfg := testConfig(t)
	d, err := NewDaemon(cfg)
	if err != nil {
		t.Fatalf("NewDaemon: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- d.Serve(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil && ctx.Err() == nil {
				t.Errorf("the daemon stopped early: %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Error("the daemon did not stop")
		}
	})
	waitSocket(t, cfg.Socket)
	s := mustSession(t, d.Manager(), "prime", "p/m")
	feed(t, d.Manager(), s.ID, 3)

	var out, errb bytes.Buffer
	if code := RunBoxctl([]string{"--socket", cfg.Socket, "events", s.ID, "--limit", "2"}, strings.NewReader(""), &out, &errb); code != 0 {
		t.Fatalf("boxctl exited %d: %s%s", code, out.String(), errb.String())
	}
	if got, want := strings.TrimSpace(errb.String()), "2 of 3 events, more with --since 3"; got != want {
		t.Fatalf("stderr = %q, want %q", got, want)
	}
	var doc struct {
		Events []Event `json:"events"`
		Next   int64   `json:"next"`
		Total  int     `json:"total"`
		More   bool    `json:"more"`
	}
	if err := json.Unmarshal(out.Bytes(), &doc); err != nil {
		t.Fatalf("boxctl printed %q: %v", out.String(), err)
	}
	if len(doc.Events) != 2 || doc.Next != 3 || doc.Total != 3 || !doc.More {
		t.Fatalf("%d event(s), next %d, total %d, more %v, want a page of 3", len(doc.Events), doc.Next, doc.Total, doc.More)
	}

	// The rest of the feed is one call away and says nothing about being cut.
	out.Reset()
	errb.Reset()
	if code := RunBoxctl([]string{"--socket", cfg.Socket, "events", s.ID, "--since", "3"}, strings.NewReader(""), &out, &errb); code != 0 {
		t.Fatalf("boxctl exited %d: %s%s", code, out.String(), errb.String())
	}
	if strings.TrimSpace(errb.String()) != "" {
		t.Fatalf("a whole feed said %q", errb.String())
	}
	if err := json.Unmarshal(out.Bytes(), &doc); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(doc.Events) != 1 || doc.More {
		t.Fatalf("%d event(s), more %v, want the last one and no more", len(doc.Events), doc.More)
	}
}

// waitSocket waits for a daemon to listen on path.
func waitSocket(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(filepath.Join(filepath.Dir(path), filepath.Base(path))); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("no socket at %s", path)
}
