package driver

import (
	"context"
	"errors"
	"fmt"
	"github.com/hashicorp/nomad/plugins/drivers"
	"nomad-driver-lima/internal/lima"
	"nomad-driver-lima/internal/repositories"
	"slices"
)

func (d *Driver) selectRepositories(tc TaskConfig) (*repositories.Selection, error) {
	c, e := repositories.Load(d.config.RepositoryProfilesFile, d.config.RepositoryProfilesSHA256)
	if e != nil {
		return nil, e
	}
	selected, e := repositories.Select(c, tc.RepositoryProfileID, tc.RepositoryProfileSHA256)
	if e != nil {
		return nil, e
	}
	if selected != nil && len(selected.Profile.Architectures) > 0 && !slices.Contains(selected.Profile.Architectures, tc.Architecture) {
		return nil, fmt.Errorf("repository profile architecture unavailable")
	}
	return selected, nil
}
func (d *Driver) applyRepositories(ctx context.Context, cfg *drivers.TaskConfig, s *State) error {
	if s.RepositoryProfile == nil {
		return nil
	}
	if e := d.stage(cfg, s, "repository_profile_apply", nil); e != nil {
		return e
	}
	receipt, e := d.client.ApplyRepositories(ctx, s.Instance, s.RepositoryProfile, lima.Identity{MachineID: s.MachineID, BootID: s.BootID})
	if e != nil {
		code := "REPOSITORY_APPLY_FAILED"
		var applyError lima.RepositoryApplyError
		if errors.As(e, &applyError) {
			code = applyError.Code
		}
		_ = d.stage(cfg, s, "repository_profile_failed", map[string]any{"error_code": code})
		return e
	}
	s.RepositoryProfile.EnvironmentSHA256 = receipt.EnvironmentSHA256
	s.RepositoryProfile.ExecutorSHA256 = receipt.ExecutorSHA256
	if _, e = s.RepositoryProfile.Reference(); e != nil {
		return fmt.Errorf("repository environment receipt invalid")
	}
	if e = d.store.write(s); e != nil {
		return e
	}
	return d.stage(cfg, s, "repository_profile_ready", nil)
}
