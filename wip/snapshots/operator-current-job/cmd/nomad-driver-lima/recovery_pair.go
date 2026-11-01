package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"nomad-driver-lima/internal/driver"
	"os"
	"syscall"
	"time"
)

type pairRuntimeConfig struct {
	StateDir        string `json:"state_dir"`
	LimaHome        string `json:"lima_home"`
	LimaBinary      string `json:"limactl"`
	RuntimeUser     string `json:"runtime_user"`
	VaultAddress    string `json:"vault_address"`
	ExpectedVersion string `json:"expected_version"`
}

func readPairInput(path string, v any) ([]byte, error) {
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, fmt.Errorf("private maintenance input unavailable")
	}
	f := os.NewFile(uintptr(fd), path)
	defer f.Close()
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() || st.Mode().Perm() != 0600 || st.Sys().(*syscall.Stat_t).Uid != uint32(os.Geteuid()) {
		return nil, fmt.Errorf("maintenance input must be current-owner regular mode0600 file")
	}
	raw, err := io.ReadAll(io.LimitReader(f, 65537))
	if err != nil || len(raw) > 65536 {
		return nil, fmt.Errorf("maintenance input too large")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err = dec.Decode(v); err != nil {
		return nil, fmt.Errorf("maintenance JSON invalid")
	}
	var extra any
	if dec.Decode(&extra) != io.EOF {
		return nil, fmt.Errorf("maintenance JSON trailing data")
	}
	return raw, nil
}
func recoveryPairMain(args []string) {
	f := flag.NewFlagSet("recover-owned-pair", flag.ExitOnError)
	var configPath, planPath, planSHA string
	var confirm bool
	f.StringVar(&configPath, "runtime-config", "", "private non-secret existing runtime configuration")
	f.StringVar(&planPath, "plan", "", "private exact original/duplicate recovery pair")
	f.StringVar(&planSHA, "plan-sha256", "", "SHA256 of exact approved plan bytes")
	f.BoolVar(&confirm, "confirm-stop-both", false, "stop both runtime processes, revoke shared task token, retain both disks; never replay a turn")
	f.Parse(args)
	var cfg pairRuntimeConfig
	var plan driver.RecoveryPairPlan
	fail := func(err error) { fmt.Fprintln(os.Stderr, err); os.Exit(1) }
	if f.NArg() != 0 {
		fail(fmt.Errorf("unexpected maintenance arguments"))
	}
	if _, err := readPairInput(configPath, &cfg); err != nil {
		fail(err)
	}
	raw, err := readPairInput(planPath, &plan)
	if err != nil {
		fail(err)
	}
	sum := sha256.Sum256(raw)
	if hex.EncodeToString(sum[:]) != planSHA {
		fail(fmt.Errorf("approved recovery plan digest mismatch"))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	result, err := driver.RecoverPair(ctx, driver.Config{StateDir: cfg.StateDir, LimaHome: cfg.LimaHome, LimaBinary: cfg.LimaBinary, RuntimeUser: cfg.RuntimeUser, VaultAddress: cfg.VaultAddress, ExpectedVersion: cfg.ExpectedVersion}, plan, confirm)
	if err != nil {
		// Phase is truthful even after partial effects; never manufacture success or
		// invite a replay. Inspect retained state/audits before a new approved plan.
		json.NewEncoder(os.Stdout).Encode(result)
		fail(err)
	}
	json.NewEncoder(os.Stdout).Encode(result)
}
