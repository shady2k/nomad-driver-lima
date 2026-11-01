package driver

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
)

// Immutable, fsynced fingerprint-only intent before each secret delivery. This
// survives a crash independently of task-monitor snapshots and never holds tokens.
type vaultExposure struct {
	Owner, Key, Allocation, Session, TokenSHA256 string
	Generation                                   uint64
}

func (s *Store) recordVaultExposure(state *State, hash string) error {
	if !recoveryKey.MatchString(hash) || !recoveryKey.MatchString(state.Key) {
		return fmt.Errorf("invalid token exposure identity")
	}
	row := vaultExposure{s.Owner, state.Key, state.AllocID, state.WorkerSessionID, hash, generation(state)}
	b, e := json.Marshal(row)
	if e != nil {
		return e
	}
	return durablePurgeAudit(filepath.Join(s.Root, state.Key+".vault-exposure."+hash+".audit"), b)
}
func (s *Store) vaultExposures(state *State) ([]string, error) {
	paths, e := filepath.Glob(filepath.Join(s.Root, state.Key+".vault-exposure.*.audit"))
	if e != nil {
		return nil, e
	}
	out := []string{}
	for _, p := range paths {
		b, e := privateRecoveryRead(p, 1<<20)
		if e != nil {
			return nil, e
		}
		var row vaultExposure
		if json.Unmarshal(b, &row) != nil || row.Owner != s.Owner || row.Key != state.Key || row.Allocation != state.AllocID || row.Session != state.WorkerSessionID || row.Generation != generation(state) || !recoveryKey.MatchString(row.TokenSHA256) || filepath.Base(p) != state.Key+".vault-exposure."+row.TokenSHA256+".audit" {
			return nil, fmt.Errorf("token exposure audit malformed")
		}
		out = append(out, row.TokenSHA256)
	}
	sort.Strings(out)
	return out, nil
}
