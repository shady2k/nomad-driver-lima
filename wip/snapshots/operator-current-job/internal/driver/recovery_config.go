package driver

import (
	"encoding/json"
	"errors"
	"fmt"
	"github.com/hashicorp/nomad/plugins/drivers"
	"io"
)

// Only attachment/cleanup inputs are persisted. Commands, Env, token values and
// secret files are deliberately excluded: recovery must never execute main.
type recoveryConfig struct {
	Version                                    int
	Allocation, Task, Namespace, Node, Session string
	CPUs, MemoryMiB, DiskGiB                   int
	VaultRequired                              bool
}

func (d *Driver) restoreAttachmentConfig(h *drivers.TaskHandle, s *State, raw string) error {
	var r recoveryConfig
	if err := json.Unmarshal([]byte(raw), &r); err != nil {
		return fmt.Errorf("invalid recovery configuration")
	}
	c := h.Config
	if r.Version != 1 || r.Allocation != s.AllocID || r.Task != s.TaskName || r.Namespace != s.Namespace || r.Node != s.NodeID || r.Session != s.WorkerSessionID || r.CPUs != s.CPUs || r.MemoryMiB != s.MemoryMiB || r.DiskGiB != s.DiskGiB || c.AllocID != s.AllocID || c.Name != s.TaskName || c.Namespace != s.Namespace || c.NodeID != s.NodeID {
		return fmt.Errorf("recovery configuration identity mismatch")
	}
	if s.CleanupVaultRequired != nil && r.VaultRequired != *s.CleanupVaultRequired {
		return fmt.Errorf("recovery secret policy mismatch")
	}
	if r.VaultRequired && (!vaultSessionID.MatchString(r.Session) || !validVaultAddress(d.config.VaultAddress)) {
		return fmt.Errorf("recovery vault configuration unavailable")
	}
	if s.TemplateCatalogSHA256 != "" {
		if s.TemplateCatalogSHA256 != d.config.CatalogSHA256 {
			return fmt.Errorf("recovery catalog pin changed")
		}
		entry, err := d.catalogEntry(TaskConfig{TemplateID: s.TemplateID, TemplateVersion: s.TemplateVersion, Architecture: s.ResolvedArchitecture, TemplateDiskSHA256: s.TemplateDiskSHA256, DiskGiB: s.DiskGiB})
		if err != nil {
			return err
		}
		if entry.YAML.SHA256 != s.TemplateYAMLSHA256 || entry.VMType != s.ResolvedVMType {
			return fmt.Errorf("recovery catalog selection changed")
		}
	}
	perCore, overhead := d.admissionCompute()
	if c.Resources == nil || c.Resources.NomadResources == nil || perCore < 1 {
		return fmt.Errorf("recovery reservation unavailable")
	}
	nr := c.Resources.NomadResources
	if !reservationCovers(int64(r.CPUs), int64(perCore), int64(r.MemoryMiB), int64(overhead), nr.Cpu.CpuShares, nr.Memory.MemoryMB) {
		return fmt.Errorf("recovery reservation mismatch")
	}
	// Copy before reconstructing the SDK's private raw configuration. No launch
	// command is reconstructed and the taskrunner's persisted handle is untouched.
	h.Config = c.Copy()
	return h.Config.EncodeConcreteDriverConfig(TaskConfig{VaultRequired: r.VaultRequired, WorkerSessionID: r.Session})
}

func missingLegacyConfig(c *drivers.TaskConfig) bool {
	var tc TaskConfig
	return errors.Is(c.DecodeDriverConfig(&tc), io.EOF)
}

func (d *Driver) restoreLegacyAttachmentConfig(h *drivers.TaskHandle, s *State) error {
	required := false
	if s.CleanupVaultRequired != nil {
		required = *s.CleanupVaultRequired
	} else {
		exposures, err := d.store.vaultExposures(s)
		if err != nil {
			return err
		}
		required = len(exposures) > 0 || s.VaultTokenSHA256 != ""
		if !required && s.WorkerSessionID != "" {
			return fmt.Errorf("legacy recovery secret policy unavailable; no launch or cleanup assumed")
		}
	}
	r := recoveryConfig{Version: 1, Allocation: s.AllocID, Task: s.TaskName, Namespace: s.Namespace, Node: s.NodeID, Session: s.WorkerSessionID, CPUs: s.CPUs, MemoryMiB: s.MemoryMiB, DiskGiB: s.DiskGiB, VaultRequired: required}
	raw, err := json.Marshal(r)
	if err != nil {
		return err
	}
	return d.restoreAttachmentConfig(h, s, string(raw))
}
