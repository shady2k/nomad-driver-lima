package driver

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"time"
)

// This is an authenticated-SSH controller protocol, not cryptographic attestation.
// The controller is trusted to perform a new real GET on each unpredictable
// challenge. Nonces bind replies to this in-flight locked recovery only.
type recoveryTarget struct {
	Instance     string `json:"instance,omitempty"`
	LedgerSHA256 string `json:"ledger_sha256,omitempty"`
	AllowGC404   bool   `json:"allow_gc_404,omitempty"`
	Key          string `json:"key"`
	AllocationID string `json:"allocation_id"`
	NodeID       string `json:"node_id"`
	JobID        string `json:"job_id"`
	Namespace    string `json:"namespace"`
}
type recoveryChallenge struct {
	Version  int            `json:"version"`
	Type     string         `json:"type"`
	Sequence int            `json:"sequence"`
	Nonce    string         `json:"nonce"`
	Target   recoveryTarget `json:"target"`
	Deadline time.Time      `json:"deadline"`
}
type recoveryObservation struct {
	Version    int              `json:"version"`
	Type       string           `json:"type"`
	Sequence   int              `json:"sequence"`
	Nonce      string           `json:"nonce"`
	Target     recoveryTarget   `json:"target"`
	ObservedAt time.Time        `json:"observed_at"`
	Allocation failedAllocation `json:"allocation"`
}
type stdioRecoveryVerifier struct {
	input    io.ReadCloser
	reader   *bufio.Reader
	output   io.Writer
	target   recoveryTarget
	sequence int
	now      func() time.Time
	timeout  time.Duration
}

func newStdioRecoveryVerifier(o FailedCreateOptions, input io.ReadCloser, output io.Writer) *stdioRecoveryVerifier {
	return &stdioRecoveryVerifier{input: input, reader: bufio.NewReaderSize(input, 64<<10), output: output, target: recoveryTarget{Instance: o.Instance, LedgerSHA256: o.ExpectedLedgerSHA256, AllowGC404: o.AllowGC404, Key: o.Key, AllocationID: o.AllocationID, NodeID: o.NodeID, JobID: o.JobID, Namespace: o.Namespace}, now: time.Now, timeout: 8 * time.Second}
}
func (v *stdioRecoveryVerifier) read(ctx context.Context) (failedAllocation, error) {
	var empty failedAllocation
	if e := ctx.Err(); e != nil {
		return empty, e
	}
	v.sequence++
	nonce := make([]byte, 32)
	if _, e := rand.Read(nonce); e != nil {
		return empty, e
	}
	sent := v.now()
	request := recoveryChallenge{1, "allocation_check", v.sequence, hex.EncodeToString(nonce), v.target, sent.Add(v.timeout)}
	if e := json.NewEncoder(v.output).Encode(request); e != nil {
		return empty, fmt.Errorf("controller challenge delivery failed; retained")
	}
	type readResult struct {
		line []byte
		err  error
	}
	ch := make(chan readResult, 1)
	go func() { line, e := v.reader.ReadSlice('\n'); ch <- readResult{append([]byte(nil), line...), e} }()
	wait, cancel := context.WithTimeout(ctx, v.timeout)
	defer cancel()
	var r readResult
	select {
	case <-wait.Done():
		v.input.Close()
		return empty, fmt.Errorf("controller observation timeout/cancellation; retained")
	case r = <-ch:
	}
	if r.err != nil {
		return empty, fmt.Errorf("controller disconnected or response oversized; retained")
	}
	var reply recoveryObservation
	dec := json.NewDecoder(bytes.NewReader(r.line))
	dec.DisallowUnknownFields()
	if e := dec.Decode(&reply); e != nil {
		return empty, fmt.Errorf("controller observation schema invalid; retained")
	}
	var extra any
	if dec.Decode(&extra) != io.EOF {
		return empty, fmt.Errorf("controller observation trailing data; retained")
	}
	now := v.now()
	if ctx.Err() != nil || now.Before(sent) || now.After(request.Deadline) || reply.Version != 1 || reply.Type != "allocation_observation" || reply.Sequence != request.Sequence || reply.Nonce != request.Nonce || reply.Target != request.Target || reply.ObservedAt.IsZero() || reply.ObservedAt.Before(sent.Add(-2*time.Second)) || reply.ObservedAt.After(now.Add(2*time.Second)) {
		return empty, fmt.Errorf("controller observation stale, replayed or mismatched; retained")
	}
	return reply.Allocation, nil
}

// RecoverFailedCreateStdio holds the same locks as local recovery. Only safe
// status fields cross the controller channel; no token, job spec or task env.
func RecoverFailedCreateStdio(ctx context.Context, o FailedCreateOptions, input io.ReadCloser, output io.Writer) (FailedCreateResult, error) {
	if o.NomadBinary != "" {
		return FailedCreateResult{}, fmt.Errorf("stdio verifier excludes local Nomad CLI")
	}
	v := newStdioRecoveryVerifier(o, input, output)
	return recoverFailedCreateRuntime(ctx, o, v.read)
}
