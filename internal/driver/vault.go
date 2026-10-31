package driver

import (
	"context"
	"fmt"
	"github.com/hashicorp/nomad/plugins/drivers"
	"io"
	"net/url"
	"nomad-driver-lima/internal/lima"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
)

var vaultSessionID = regexp.MustCompile(`^[a-f0-9]{8}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{12}$`)

func readVaultToken(cfg *drivers.TaskConfig) (string, error) {
	if cfg.AllocDir == "" || filepath.Base(cfg.Name) != cfg.Name {
		return "", fmt.Errorf("task secrets directory unavailable")
	}
	p := filepath.Join(cfg.TaskDir().SecretsDir, "vault_token")
	fd, e := syscall.Open(p, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if e != nil {
		return "", fmt.Errorf("task vault token unavailable")
	}
	f := os.NewFile(uintptr(fd), p)
	defer f.Close()
	st, e := f.Stat()
	if e != nil || !st.Mode().IsRegular() {
		return "", fmt.Errorf("invalid task vault token file")
	}
	b, e := io.ReadAll(io.LimitReader(f, 16385))
	if e != nil || len(b) > 16384 {
		return "", fmt.Errorf("invalid task vault token size")
	}
	s := strings.TrimSpace(string(b))
	if s == "" {
		return "", fmt.Errorf("empty task vault token")
	}
	for _, v := range s {
		if v <= 32 || v >= 127 {
			return "", fmt.Errorf("invalid task vault token")
		}
	}
	return s, nil
}
func (d *Driver) syncVault(ctx context.Context, cfg *drivers.TaskConfig, s *State, previous string) (string, error) {
	var tc TaskConfig
	if e := cfg.DecodeDriverConfig(&tc); e != nil {
		return "", e
	}
	if !tc.VaultRequired {
		return "", nil
	}
	if !vaultSessionID.MatchString(s.WorkerSessionID) {
		return "", fmt.Errorf("vault requires canonical worker session")
	}
	if s.MachineID == "" || s.BootID == "" {
		return "", fmt.Errorf("guest identity required before task token delivery")
	}
	_, ident, e := d.client.Guest(ctx, s.Instance, d.payload(s, "identity"))
	if e != nil || ident.MachineID != s.MachineID || ident.BootID != s.BootID {
		return "", fmt.Errorf("guest identity changed; task token retained on host")
	}
	token, e := readVaultToken(cfg)
	if e != nil {
		return "", e
	}
	hash := lima.Hash([]byte(token))
	if hash == previous {
		return hash, nil
	}
	if s.Key != "" {
		if e = d.freshResumeToken(s, hash); e != nil {
			return "", e
		}
		if e = d.store.recordVaultExposure(s, hash); e != nil {
			return "", e
		}
	}
	if e = d.client.VaultToken(ctx, s.Instance, s.WorkerSessionID, s.Owner, "sync", token); e != nil {
		return "", e
	}
	s.VaultTokenSHA256 = hash
	return hash, nil
}
func (d *Driver) clearVault(ctx context.Context, cfg *drivers.TaskConfig, s *State) error {
	var tc TaskConfig
	if e := cfg.DecodeDriverConfig(&tc); e != nil {
		return e
	}
	if !tc.VaultRequired {
		return nil
	}
	return d.client.VaultToken(ctx, s.Instance, s.WorkerSessionID, s.Owner, "clear", "")
}

func validVaultAddress(address string) bool {
	u, e := url.Parse(address)
	return e == nil && u.Scheme == "https" && u.Hostname() != "" && u.User == nil && u.RawQuery == "" && u.Fragment == "" && (u.Path == "" || u.Path == "/")
}
func (d *Driver) finalizeVault(ctx context.Context, cfg *drivers.TaskConfig, s *State) error {
	var tc TaskConfig
	if e := cfg.DecodeDriverConfig(&tc); e != nil {
		return e
	}
	if !tc.VaultRequired {
		return nil
	}
	if !validVaultAddress(d.config.VaultAddress) {
		return fmt.Errorf("operator HTTPS vault_address required")
	}
	if e := d.client.RevokeVault(ctx, s.Instance, s.WorkerSessionID, s.Owner, d.config.VaultAddress); e != nil {
		return fmt.Errorf("task token revoke outcome unconfirmed; VM/data retained: %w", e)
	}
	return d.clearVault(ctx, cfg, s)
}
