package driver

import (
	"fmt"
	"nomad-driver-lima/internal/catalog"
	"os"
	"path/filepath"
	"strings"
)

// An operator-pinned snapshot of the original catalog is an attachment receipt,
// never an additional launch catalog. No caller/task may supply this path/pin.
func validateRecoveryCatalogConfig(c Config) error {
	if c.RecoveryCatalogFile == "" && c.RecoveryCatalogSHA256 == "" {
		return nil
	}
	if !filepath.IsAbs(c.RecoveryCatalogFile) || !recoveryKey.MatchString(c.RecoveryCatalogSHA256) {
		return fmt.Errorf("paired absolute recovery catalog receipt and checksum required")
	}
	if filepath.Clean(filepath.Dir(c.RecoveryCatalogFile)) == filepath.Clean(c.StateDir) && strings.EqualFold(filepath.Ext(c.RecoveryCatalogFile), ".json") {
		return fmt.Errorf("recovery catalog receipt must not collide with state JSON ledger")
	}
	st, e := os.Lstat(c.RecoveryCatalogFile)
	if e != nil || !st.Mode().IsRegular() {
		return fmt.Errorf("regular nonsymlink recovery catalog receipt required")
	}
	_, e = catalog.Read(c.RecoveryCatalogFile, c.RecoveryCatalogSHA256)
	return e
}
func (d *Driver) recoveryValidator(s *State) (*Driver, error) {
	d.configMu.RLock()
	cfg, perCore := d.config, d.mhzPerCore
	d.configMu.RUnlock()
	if s.TemplateCatalogSHA256 != "" && s.TemplateCatalogSHA256 != cfg.CatalogSHA256 {
		if s.TemplateCatalogSHA256 != cfg.RecoveryCatalogSHA256 || cfg.RecoveryCatalogFile == "" {
			return nil, fmt.Errorf("original recovery catalog receipt unavailable")
		}
		if e := validateRecoveryCatalogConfig(cfg); e != nil {
			return nil, e
		}
		cfg.CatalogFile, cfg.CatalogSHA256 = cfg.RecoveryCatalogFile, cfg.RecoveryCatalogSHA256
	}
	// New independent locks: never copy a live Driver's mutexes. This view is used
	// solely for validation/selection, never StartTask, create, main or a monitor.
	return &Driver{config: cfg, mhzPerCore: perCore, store: d.store, client: d.client, runtimeIdentity: d.runtimeIdentity}, nil
}
func (d *Driver) verifyRecoveryCatalog(s *State) error {
	if s.TemplateCatalogSHA256 == "" {
		return nil
	}
	view, e := d.recoveryValidator(s)
	if e != nil {
		return e
	}
	entry, e := view.catalogEntry(TaskConfig{TemplateID: s.TemplateID, TemplateVersion: s.TemplateVersion, Architecture: s.ResolvedArchitecture, TemplateDiskSHA256: s.TemplateDiskSHA256, DiskGiB: s.DiskGiB})
	if e != nil {
		return e
	}
	if entry.YAML.SHA256 != s.TemplateYAMLSHA256 || entry.VMType != s.ResolvedVMType {
		return fmt.Errorf("original recovery catalog selection changed")
	}
	return nil
}
