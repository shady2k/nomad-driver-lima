package lima

import (
	"context"
	"encoding/json"
	"fmt"
	"nomad-driver-lima/internal/repositories"
	guestassets "nomad-driver-lima/plugin/guest"
	"regexp"
)

type RepositoryApplyError struct{ Code string }

func (e RepositoryApplyError) Error() string { return "repository profile apply rejected: " + e.Code }

type RepositoryReceipt struct {
	Enabled           bool   `json:"enabled"`
	ProfileSHA256     string `json:"profile_sha256"`
	EnvironmentSHA256 string `json:"environment_sha256"`
	ExecutorSHA256    string `json:"executor_sha256"`
}

func (c *Client) ApplyRepositories(ctx context.Context, name string, s *repositories.Selection, id Identity) (RepositoryReceipt, error) {
	var result RepositoryReceipt
	if s == nil {
		return result, nil
	}
	if !validName.MatchString(name) {
		return result, fmt.Errorf("invalid repository guest")
	}
	payload := map[string]any{"profile": s.Profile, "profile_sha256": s.SHA256, "expected_guest_identity": map[string]string{"machine_id": id.MachineID, "boot_id": id.BootID}, "executor_source": guestassets.RepositoryProfile, "executor_sha256": Hash([]byte(guestassets.RepositoryProfile))}
	raw, e := json.Marshal(payload)
	if e != nil {
		return result, e
	}
	out, e := c.Runner.Run(ctx, raw, "shell", "--workdir=/", name, "--", "sudo", "-n", "/usr/bin/python3", "-c", guestassets.RepositoryProfile)
	if e != nil {
		var failure struct {
			Error string `json:"error"`
		}
		code := "REPOSITORY_APPLY_TRANSPORT_OR_PROTOCOL_ERROR"
		if json.Unmarshal(out, &failure) == nil && regexp.MustCompile(`^[A-Z_]{1,96}$`).MatchString(failure.Error) {
			code = failure.Error
		}
		return result, RepositoryApplyError{Code: code}
	}
	if json.Unmarshal(out, &result) != nil || !result.Enabled || result.ProfileSHA256 != s.SHA256 || result.ExecutorSHA256 != Hash([]byte(guestassets.RepositoryProfile)) {
		return result, fmt.Errorf("repository apply receipt invalid")
	}
	return result, nil
}
func RepositoryCommand(s *repositories.Selection, command []string) ([]string, error) {
	if s == nil {
		return command, nil
	}
	reference, e := s.Reference()
	if e != nil {
		return nil, e
	}
	raw, _ := json.Marshal(reference)
	program := guestassets.RepositoryExec + "\nimport json,os,sys\ncommand=repository_bound_command(json.loads(sys.argv[1]),sys.argv[2:])\nos.execvpe(command[0],command,os.environ)\n"
	return append([]string{"/usr/bin/python3", "-c", program, string(raw)}, command...), nil
}
