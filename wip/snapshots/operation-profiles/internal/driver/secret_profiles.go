package driver

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/hashicorp/nomad/plugins/drivers"
	"io"
	"nomad-driver-lima/internal/lima"
	"os"
	"path/filepath"
	"regexp"
	"syscall"
)

func loadSecretProfiles(path, hash string) (json.RawMessage, error) {
	if path == "" {
		if hash != "" {
			return nil, fmt.Errorf("secret profile file required")
		}
		return nil, nil
	}
	if !filepath.IsAbs(path) || !regexp.MustCompile(`^[a-f0-9]{64}$`).MatchString(hash) {
		return nil, fmt.Errorf("absolute pinned secret profile file required")
	}
	fd, e := syscall.Open(path, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if e != nil {
		return nil, fmt.Errorf("secret profile config unavailable")
	}
	f := os.NewFile(uintptr(fd), path)
	defer f.Close()
	st, e := f.Stat()
	if e != nil || !st.Mode().IsRegular() || st.Mode().Perm()&0022 != 0 || st.Size() > 262144 {
		return nil, fmt.Errorf("unsafe secret profile config")
	}
	if stat, ok := st.Sys().(*syscall.Stat_t); !ok || int(stat.Uid) != os.Geteuid() {
		return nil, fmt.Errorf("secret profile config owner mismatch")
	}
	raw, e := io.ReadAll(io.LimitReader(f, 262145))
	if e != nil || len(raw) > 262144 || lima.Hash(raw) != hash {
		return nil, fmt.Errorf("secret profile config digest mismatch")
	}
	var c struct {
		Schema   int               `json:"schema_version"`
		Profiles []json.RawMessage `json:"profiles"`
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if d.Decode(&c) != nil || d.Decode(new(any)) != io.EOF || c.Schema != 1 || len(c.Profiles) < 1 || len(c.Profiles) > 32 {
		return nil, fmt.Errorf("secret profile config schema invalid")
	}
	// Full bounded profile validation is performed by the embedded first-party
	// delivery program before any workload may use it. No secret path is fetched.
	return raw, nil
}
func (d *Driver) pinSecretProfiles(cfg *drivers.TaskConfig, tc TaskConfig, s *State) error {
	if !tc.VaultRequired {
		return nil
	}
	raw, e := loadSecretProfiles(d.config.SecretProfilesFile, d.config.SecretProfilesSHA256)
	if e != nil {
		return e
	}
	if len(raw) > 0 {
		s.SecretProfiles = raw
		s.SecretBaoBin = d.config.SecretProfilesBaoBin
		s.SecretVaultAddress = d.config.VaultAddress
	}
	return nil
}
func (d *Driver) deliverSecretProfiles(ctx context.Context, cfg *drivers.TaskConfig, s *State) error {
	if len(s.SecretProfiles) == 0 {
		return nil
	}
	generation := cfg.Env["NOMAD_META_creator_generation"]
	if generation == "" {
		generation = cfg.Env["NOMAD_META_CREATOR_GENERATION"]
	}
	if !vaultSessionID.MatchString(generation) || s.CreatorAgentID == "" || s.CreatorSessionID == "" {
		return fmt.Errorf("secret profiles require creator generation identity")
	}
	p := map[string]any{"catalogue": json.RawMessage(s.SecretProfiles), "namespace": cfg.Namespace, "task": cfg.Name, "address": s.SecretVaultAddress, "bao_bin": s.SecretBaoBin, "machine_identity": map[string]string{"machine_id": s.MachineID, "boot_id": s.BootID}, "binding": map[string]any{"session_id": s.WorkerSessionID, "attempt": map[string]string{"allocation": s.AllocID, "task": cfg.Name}, "creator_agent_id": s.CreatorAgentID, "creator_session_id": s.CreatorSessionID, "creator_generation": generation}}
	return d.client.DeliverSecretProfiles(ctx, s.Instance, p)
}
