package driver

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"
)

func TestStdioRecoveryFreshChecksKeepLocksAndRejectReplay(t *testing.T) {
	for _, kind := range []string{"success", "replay", "identity", "stale", "future", "disconnect", "timeout", "changed-allocation", "active", "partial-disk", "active-process"} {
		t.Run(kind, func(t *testing.T) {
			o, store, _, d := failedRecoveryFixture(t)
			reader, controller := io.Pipe()
			requests, writer := io.Pipe()
			defer reader.Close()
			defer controller.Close()
			defer requests.Close()
			defer writer.Close()
			verifier := newStdioRecoveryVerifier(o, reader, writer)
			verifier.timeout = 100 * time.Millisecond
			read := d.allocation
			d.allocation = verifier.read
			if kind == "partial-disk" || kind == "active-process" {
				d.absent = func(context.Context, *State) error { return io.ErrUnexpectedEOF }
			}
			done := make(chan error, 1)
			go func() { _, e := recoverFailedCreate(t.Context(), o, d); done <- e }()
			decoder := json.NewDecoder(requests)
			var prior recoveryObservation
			replies := 0
			for sequence := 1; sequence <= 2; sequence++ {
				var challenge recoveryChallenge
				if e := decoder.Decode(&challenge); e != nil {
					break
				}
				// Both locks remain owned by the runtime during each remote real GET.
				if lock, e := store.lock(o.Key); e == nil {
					unlock(lock)
					t.Fatal("task lock released during verification")
				}
				// Admission lock is also held. Name is the same as fresh StartTask.
				if kind == "disconnect" {
					controller.Close()
					break
				}
				if kind == "timeout" {
					break
				}
				a, _ := read(t.Context())
				if kind == "changed-allocation" {
					a.ModifyIndex += uint64(sequence)
				}
				if kind == "active" {
					a.ClientStatus = "running"
				}
				reply := recoveryObservation{1, "allocation_observation", sequence, challenge.Nonce, challenge.Target, time.Now(), a}
				if kind == "identity" {
					reply.Target.JobID = "other"
				}
				if kind == "stale" {
					reply.ObservedAt = time.Now().Add(-time.Minute)
				}
				if kind == "future" {
					reply.ObservedAt = time.Now().Add(time.Minute)
				}
				if kind == "replay" && sequence == 2 {
					reply = prior
				}
				prior = reply
				if e := json.NewEncoder(controller).Encode(reply); e != nil {
					break
				}
				replies++
				if kind != "success" && kind != "replay" && kind != "changed-allocation" {
					break
				}
			}
			select {
			case e := <-done:
				if kind == "success" {
					if e != nil || replies != 2 {
						t.Fatal(e, replies)
					}
				} else {
					if e == nil {
						t.Fatal("invalid observation accepted")
					}
					assertRecoveryRetained(t, o, store)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("stdio recovery did not fail closed promptly")
			}
		})
	}
}
func TestStdioRecoveryBoundedAndUnknownFields(t *testing.T) {
	for _, payload := range []string{strings.Repeat("x", 70<<10) + "\n", "{\"secret\":\"not-accepted\"}\n"} {
		o, _, _, _ := failedRecoveryFixture(t)
		v := newStdioRecoveryVerifier(o, io.NopCloser(strings.NewReader(payload)), io.Discard)
		if _, e := v.read(t.Context()); e == nil {
			t.Fatal("bad schema/oversized response accepted")
		}
	}
}
func TestStdioRecoveryAlreadyCommittedNeedsNoNewObservation(t *testing.T) {
	o, _, _, d := failedRecoveryFixture(t)
	first, e := recoverFailedCreate(t.Context(), o, d)
	if e != nil {
		t.Fatal(e)
	}
	d.allocation = func(context.Context) (failedAllocation, error) {
		t.Fatal("replayed verification after commit")
		return failedAllocation{}, nil
	}
	next, e := recoverFailedCreate(t.Context(), o, d)
	if e != nil || !next.AlreadyRecovered || next.AuditSHA256 != first.AuditSHA256 {
		t.Fatal(next, e)
	}
}
func TestStdioReadCancelledClosesInput(t *testing.T) {
	o, _, _, _ := failedRecoveryFixture(t)
	reader, writer := io.Pipe()
	defer writer.Close()
	v := newStdioRecoveryVerifier(o, reader, bufio.NewWriter(io.Discard))
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, e := v.read(ctx); e == nil {
		t.Fatal("cancelled read accepted")
	}
	reader.Close()
}
