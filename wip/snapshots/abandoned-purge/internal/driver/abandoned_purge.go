package driver

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

// Only metadata from the current Job GET, never the embedded historical Job.
type purgeJobProof struct {
	AbsenceVerified                                         bool
	NamespaceJobsHTTPStatus, NamespaceAllocationsHTTPStatus int
	ModifyIndex                                             uint64
	HTTPStatus                                              int
	ID, Namespace, Status, CreatorAgentID, CreatorSessionID string
	Stop                                                    bool
}

// The caller already holds the exact task and node-admission locks. Refuse
// external/symlink layouts and any dependent qcow2 chain; never inspect payloads.
func stoppedPurgeDependencies(ctx context.Context, o FailedCreateOptions, s *State) error {
	if !filepath.IsAbs(o.QemuImg) {
		return fmt.Errorf("absolute existing qemu-img required")
	}
	target := filepath.Join(o.LimaHome, s.Instance, "disk")
	targetInfo, e := os.Lstat(target)
	if os.IsNotExist(e) {
		return nil
	} // Existing intent plus fresh instance/process absence is verified by purgeRetained.
	if e != nil || !targetInfo.Mode().IsRegular() {
		return fmt.Errorf("regular target disk required")
	}
	entries, e := os.ReadDir(o.LimaHome)
	if e != nil {
		return e
	}
	for _, entry := range entries {
		if entry.Name() == s.Instance {
			continue
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("ambiguous instance symlink; retain")
		}
		if !entry.IsDir() {
			continue
		}
		for _, name := range []string{"disk", "diffdisk", "basedisk"} {
			p := filepath.Join(o.LimaHome, entry.Name(), name)
			st, er := os.Lstat(p)
			if os.IsNotExist(er) {
				continue
			}
			if er != nil || !st.Mode().IsRegular() {
				return fmt.Errorf("ambiguous disk layout; retain")
			}
			if os.SameFile(targetInfo, st) {
				return fmt.Errorf("another instance shares target disk; retain")
			}
			command := exec.CommandContext(ctx, o.QemuImg, "info", "--force-share", "--output=json", "--backing-chain", p)
			var out limitedPurgeOutput
			command.Stdout = &out
			command.Stderr = &limitedPurgeOutput{}
			if command.Run() != nil {
				return fmt.Errorf("backing chain unavailable; retain")
			}
			var chain []struct {
				Filename string `json:"filename"`
				Backing  string `json:"full-backing-filename"`
			}
			if json.Unmarshal(out.Bytes(), &chain) != nil || len(chain) == 0 {
				return fmt.Errorf("invalid backing chain; retain")
			}
			for _, v := range chain {
				for _, f := range []string{v.Filename, v.Backing} {
					if f == "" {
						continue
					}
					if !filepath.IsAbs(f) {
						f = filepath.Join(filepath.Dir(p), f)
					}
					resolved, er := filepath.EvalSymlinks(f)
					if er != nil {
						return fmt.Errorf("unresolved backing file; retain")
					}
					info, er := os.Stat(resolved)
					if er != nil {
						return fmt.Errorf("unreadable backing file; retain")
					}
					if os.SameFile(targetInfo, info) {
						return fmt.Errorf("dependent backing chain uses target; retain")
					}
				}
			}
		}
	}
	return nil
}

type limitedPurgeOutput struct{ bytes.Buffer }

func (b *limitedPurgeOutput) Write(p []byte) (int, error) {
	if b.Len()+len(p) > 1<<20 {
		return 0, fmt.Errorf("metadata bound exceeded")
	}
	return b.Buffer.Write(p)
}
