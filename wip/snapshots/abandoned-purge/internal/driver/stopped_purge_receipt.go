package driver

import (
	"encoding/json"
	"fmt"
	"nomad-driver-lima/internal/lima"
)

type stoppedPurgeReceipt struct {
	Purpose           string `json:"purpose,omitempty"`
	Allocation        string `json:"allocation"`
	Instance          string `json:"instance"`
	Session           string `json:"session"`
	VMStatus          string `json:"vm_status"`
	DiskRetained      bool   `json:"disk_retained"`
	DiskSizeBytes     int64  `json:"disk_size_bytes"`
	DiskSHA256        string `json:"disk_sha256"`
	LedgerUnchanged   bool   `json:"ledger_unchanged"`
	LedgerSHA256      string `json:"ledger_sha256"`
	Revocation        string `json:"revocation"`
	ApprovalReference string `json:"approval_reference"`
	RuntimeStop       struct {
		OK         bool   `json:"ok"`
		Stopped    bool   `json:"runtime_stopped"`
		Revocation string `json:"revocation"`
	} `json:"runtime_stop"`
}

func validateStoppedPurgeReceipt(o FailedCreateOptions, s *State, original []byte) ([]byte, error) {
	if !recoveryKey.MatchString(o.ExpectedLedgerSHA256) || !recoveryKey.MatchString(o.StoppedSourceReceiptSHA256) {
		return nil, fmt.Errorf("exact stopped receipt and original ledger hashes required")
	}
	raw, e := privateRecoveryRead(o.StoppedSourceReceipt, 1<<20)
	if e != nil || lima.Hash(raw) != o.StoppedSourceReceiptSHA256 {
		return nil, fmt.Errorf("approved private stopped receipt changed")
	}
	var r stoppedPurgeReceipt
	if json.Unmarshal(raw, &r) != nil || r.Allocation != s.AllocID || r.Instance != s.Instance || r.Session != s.WorkerSessionID || r.VMStatus != "Stopped" || !r.DiskRetained || !r.LedgerUnchanged || r.LedgerSHA256 != o.ExpectedLedgerSHA256 || r.Revocation != "unconfirmed" || r.ApprovalReference == "" || !r.RuntimeStop.OK || !r.RuntimeStop.Stopped || r.RuntimeStop.Revocation != "not_attempted" || !recoveryKey.MatchString(r.DiskSHA256) || r.DiskSizeBytes < 1 {
		return nil, fmt.Errorf("exact stopped/unconfirmed source proof required")
	}
	if o.AbandonedStopped {
		if r.Purpose != "abandoned_stopped_purge" || (s.Phase != "running" && s.Phase != "exited" && s.Phase != "stopped" && s.Phase != "purge_pending") {
			return nil, fmt.Errorf("explicit abandoned stopped purge proof required")
		}
	} else if !s.VaultFinalizing || s.Exit == nil || (s.Phase != "exited" && s.Phase != "purge_pending") {
		return nil, fmt.Errorf("failed finalizing source required; no fabricated finish")
	}
	if s.Phase != "purge_pending" && lima.Hash(original) != o.ExpectedLedgerSHA256 {
		return nil, fmt.Errorf("stopped source ledger changed")
	}
	return raw, nil
}
