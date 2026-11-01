package driver

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"nomad-driver-lima/internal/lima"
	"nomad-driver-lima/internal/repositories"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

type RetainedDiskIdentity struct {
	Name     string
	Dev, Ino uint64
	Size     int64
}
type StopReceipt struct {
	ExposedTokenSHA256                         []string
	Disk                                       *RetainedDiskIdentity
	Allocation, Session, Instance, TokenSHA256 string
	Generation                                 uint64
	ConfirmedAt                                time.Time
	RevocationConfirmed                        bool
}
type State struct {
	SecretProfiles     json.RawMessage         `json:"SecretProfiles,omitempty"`
	SecretBaoBin       string                  `json:"SecretBaoBin,omitempty"`
	SecretVaultAddress string                  `json:"SecretVaultAddress,omitempty"`
	Finalization       *FinalizationState      `json:"Finalization,omitempty"`
	PrivateCopy        *PrivateCopyAdmission   `json:"PrivateCopy,omitempty"`
	RepositoryProfile  *repositories.Selection `json:"RepositoryProfile,omitempty"`
	// ResumeNext is an irreversible, durable transfer intent containing the exact
	// successor. Its existence fences the source even before successor materialization.
	ResumeNext                                                                                                    *State
	ResumedFrom, InstanceKey, NodeID, Namespace, ResumeMachineID, ResumePreviousBootID                            string
	Generation                                                                                                    uint64
	StopReceipt                                                                                                   *StopReceipt
	VaultTokenSHA256                                                                                              string
	TemplateCatalogSHA256, TemplateYAMLSHA256, ResolvedArchitecture, ResolvedVMType                               string
	PurgedDiskGiB                                                                                                 int
	PurgeIntentSHA256, PurgeAuditSHA256                                                                           string
	FailedCreateAuditSHA256                                                                                       string
	VaultFinalizing                                                                                               bool
	StageName                                                                                                     string
	StageStartedAt                                                                                                time.Time
	WorkerSessionID                                                                                               string
	Lifecycle                                                                                                     ActivityObservation
	CreatorAgentID, CreatorSessionID, TemplateID, TemplateVersion, TemplateDiskSHA256, Purpose, Project, TaskLink string
	Version                                                                                                       int
	Key, Owner, TaskID, AllocID, TaskName, Instance, SpecHash, ConfigHash, MachineID, BootID, Phase               string
	CPUs, MemoryMiB, DiskGiB                                                                                      int
	StartedAt, CompletedAt                                                                                        time.Time
	Exit                                                                                                          *lima.Result
	OutOffset, ErrOffset                                                                                          int64
	Destroyed                                                                                                     bool
}
type Store struct{ Root, Owner string }

func openStore(root string) (*Store, error) {
	if !filepath.IsAbs(root) {
		return nil, fmt.Errorf("absolute state directory required")
	}
	if e := os.MkdirAll(root, 0700); e != nil {
		return nil, e
	}
	s, e := os.Lstat(root)
	if e != nil || !s.IsDir() || s.Mode().Perm()&0077 != 0 || s.Sys().(*syscall.Stat_t).Uid != uint32(os.Getuid()) {
		return nil, fmt.Errorf("private owned state directory required")
	}
	p := filepath.Join(root, "owner")
	b, e := os.ReadFile(p)
	if os.IsNotExist(e) {
		b = make([]byte, 16)
		if _, e = rand.Read(b); e != nil {
			return nil, e
		}
		b = []byte(hex.EncodeToString(b))
		f, e := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if e != nil {
			return nil, e
		}
		_, e = f.Write(b)
		f.Sync()
		f.Close()
		if e != nil {
			return nil, e
		}
	} else if e != nil {
		return nil, e
	}
	if len(b) != 32 {
		return nil, fmt.Errorf("invalid store owner")
	}
	return &Store{root, string(b)}, nil
}
func (s *Store) read(key string) (*State, error) {
	if len(key) != 64 {
		return nil, fmt.Errorf("invalid state key")
	}
	p := filepath.Join(s.Root, key+".json")
	fd, e := syscall.Open(p, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if e != nil {
		return nil, e
	}
	f := os.NewFile(uintptr(fd), p)
	defer f.Close()
	st, e := f.Stat()
	if e != nil || !st.Mode().IsRegular() || st.Mode().Perm() != 0600 {
		return nil, fmt.Errorf("invalid state file")
	}
	var v State
	if e = json.NewDecoder(f).Decode(&v); e != nil {
		return nil, e
	}
	if (v.Version < 1 || v.Version > 5) || (v.RepositoryProfile != nil && v.Version != 2 && v.Version != 3 && v.Version != 4 && v.Version != 5) || (v.PrivateCopy != nil && v.Version != 3 && v.Version != 4 && v.Version != 5) || (v.Finalization != nil && v.Version != 4 && v.Version != 5) || (v.Version == 4 && v.Finalization == nil) || (len(v.SecretProfiles) > 0 && v.Version != 5) || (v.Version == 5 && len(v.SecretProfiles) == 0) || v.Key != key || v.Owner != s.Owner {
		return nil, fmt.Errorf("state identity mismatch")
	}
	if v.Finalization != nil && !validFinalizationState(&v) {
		return nil, fmt.Errorf("finalization state binding invalid")
	}
	return &v, nil
}
func (s *Store) write(v *State) error {
	if len(v.SecretProfiles) > 0 {
		v.Version = 5
	}
	if v.Owner != s.Owner || len(v.Key) != 64 {
		return fmt.Errorf("invalid write identity")
	}
	// Corrupt/unreadable state is not absence, and transferred history is immutable.
	old, err := s.read(v.Key)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if err == nil && old.ResumeNext != nil {
		previous, _ := json.Marshal(old)
		replacement, _ := json.Marshal(v)
		if string(previous) != string(replacement) {
			return fmt.Errorf("task ownership transferred; historical write rejected")
		}
	}
	b, e := json.Marshal(v)
	if e != nil {
		return e
	}
	f, e := os.CreateTemp(s.Root, ".state-")
	if e != nil {
		return e
	}
	defer os.Remove(f.Name())
	if _, e = f.Write(b); e == nil {
		e = f.Sync()
	}
	f.Close()
	if e != nil {
		return e
	}
	if e = os.Rename(f.Name(), filepath.Join(s.Root, v.Key+".json")); e != nil {
		return e
	}
	d, e := os.Open(s.Root)
	if e == nil {
		defer d.Close()
		return d.Sync()
	}
	return e
}
func (s *Store) lock(key string) (*os.File, error) { return s.lockMode(key, syscall.LOCK_EX) }
func (s *Store) lockMode(key string, mode int) (*os.File, error) {
	p := filepath.Join(s.Root, key+".lock")
	fd, e := syscall.Open(p, syscall.O_CREAT|syscall.O_RDWR|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0600)
	if e != nil {
		return nil, e
	}
	f := os.NewFile(uintptr(fd), p)
	if e = syscall.Flock(fd, mode|syscall.LOCK_NB); e != nil {
		f.Close()
		return nil, fmt.Errorf("task already owned by live driver: %w", e)
	}
	return f, nil
}
func unlock(f *os.File) { syscall.Flock(int(f.Fd()), syscall.LOCK_UN); f.Close() }
func physicalFree(path string, guestGiB, floorMiB int) error {
	var st syscall.Statfs_t
	if e := syscall.Statfs(path, &st); e != nil {
		return e
	}
	if guestGiB < 1 || guestGiB > 1048576 || floorMiB < 0 || floorMiB > 1048576 {
		return fmt.Errorf("invalid disk budget")
	}
	need := uint64(guestGiB)*1073741824 + uint64(floorMiB)*1048576
	if uint64(st.Bavail)*uint64(st.Bsize) < need {
		return fmt.Errorf("physical_disk_space_insufficient")
	}
	return nil
}
