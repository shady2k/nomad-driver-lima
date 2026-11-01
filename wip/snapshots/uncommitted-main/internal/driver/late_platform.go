package driver

import (
	"encoding/json"
	"fmt"
	"io"
	"nomad-driver-lima/internal/catalog"
	"runtime"
	"strings"
)

// Pins only, never caller-selected paths or URLs. Catalog remains operator-owned.
type platformPin struct {
	Architecture   string `json:"architecture"`
	GuestOS        string `json:"guest_os"`
	VMType         string `json:"vm_type"`
	DiskSHA256     string `json:"disk_sha256"`
	YAMLSHA256     string `json:"yaml_sha256"`
	MinimumDiskGiB int    `json:"minimum_disk_gib"`
}

func resolvePin(c *catalog.Catalog, tc TaskConfig, arch, hostOS string) (catalog.Entry, error) {
	vm := "qemu"
	if hostOS == "darwin" {
		vm = "vz"
	} else if hostOS != "linux" {
		return catalog.Entry{}, fmt.Errorf("unsupported host OS")
	}
	if tc.Architecture != "" && tc.Architecture != "auto" && tc.Architecture != arch {
		return catalog.Entry{}, fmt.Errorf("explicit architecture differs from placed node")
	}
	if len(tc.TemplateVariantsJSON) > 131072 {
		return catalog.Entry{}, fmt.Errorf("variant set too large")
	}
	var pins []platformPin
	dec := json.NewDecoder(strings.NewReader(tc.TemplateVariantsJSON))
	dec.DisallowUnknownFields()
	if e := dec.Decode(&pins); e != nil || len(pins) < 1 || len(pins) > 32 {
		return catalog.Entry{}, fmt.Errorf("invalid pinned variant set")
	}
	if e := dec.Decode(new(any)); e != io.EOF {
		return catalog.Entry{}, fmt.Errorf("variant trailing data")
	}
	seen := map[string]bool{}
	var selected catalog.Entry
	found := false
	for _, p := range pins {
		key := p.Architecture + "/" + p.VMType
		if seen[key] {
			return selected, fmt.Errorf("duplicate variant")
		}
		seen[key] = true
		e, err := c.Select(tc.TemplateID, tc.TemplateVersion, p.Architecture, p.VMType)
		if err != nil || p.GuestOS != e.GuestOS || p.DiskSHA256 != e.Disk.SHA256 || p.YAMLSHA256 != e.YAML.SHA256 || p.MinimumDiskGiB != e.Disk.MinimumDiskGiB || tc.DiskGiB < p.MinimumDiskGiB {
			return selected, fmt.Errorf("variant differs from pinned catalog or disk minimum")
		}
		if p.Architecture == arch && p.VMType == vm {
			selected = e
			found = true
		}
	}
	if !found {
		return selected, fmt.Errorf("placed node has no approved pinned variant")
	}
	return selected, nil
}
func (d *Driver) resolvePlatform(tc *TaskConfig) error {
	if tc.TemplateVariantsJSON == "" {
		if tc.TemplateCatalogSHA256 != "" && tc.TemplateCatalogSHA256 != d.config.CatalogSHA256 {
			return fmt.Errorf("task catalog pin differs from node")
		}
		return nil
	}
	if tc.TemplateDiskSHA256 != "" || tc.TemplateCatalogSHA256 == "" || tc.TemplateCatalogSHA256 != d.config.CatalogSHA256 {
		return fmt.Errorf("late selection requires exact catalog pin and no preselected disk")
	}
	c, e := catalog.Read(d.config.CatalogFile, tc.TemplateCatalogSHA256)
	if e != nil {
		return e
	}
	entry, e := resolvePin(c, *tc, runtime.GOARCH, runtime.GOOS)
	if e != nil {
		return e
	}
	tc.Architecture = entry.Architecture
	tc.TemplateDiskSHA256 = entry.Disk.SHA256
	tc.resolvedYAMLSHA256 = entry.YAML.SHA256
	tc.resolvedVMType = entry.VMType
	return nil
}
func verifyResolvedState(tc TaskConfig, s *State) error {
	if tc.TemplateVariantsJSON == "" {
		return nil
	}
	if s.TemplateCatalogSHA256 != tc.TemplateCatalogSHA256 || s.TemplateDiskSHA256 != tc.TemplateDiskSHA256 || s.TemplateYAMLSHA256 != tc.resolvedYAMLSHA256 || s.ResolvedArchitecture != tc.Architecture || s.ResolvedVMType != tc.resolvedVMType {
		return fmt.Errorf("durable platform selection changed; no reselect")
	}
	return nil
}
func selectionAnnotations(s *State, phase string) map[string]string {
	out := map[string]string{"phase": phase, "instance": s.Instance}
	if s.TemplateCatalogSHA256 != "" {
		out["template_catalog_sha256"] = s.TemplateCatalogSHA256
		out["template_disk_sha256"] = s.TemplateDiskSHA256
		out["template_yaml_sha256"] = s.TemplateYAMLSHA256
		out["resolved_architecture"] = s.ResolvedArchitecture
		out["resolved_vm_type"] = s.ResolvedVMType
		out["resolved_guest_os"] = "linux"
	}
	return out
}
