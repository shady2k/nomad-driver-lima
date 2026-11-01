package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	log "github.com/hashicorp/go-hclog"
	"github.com/hashicorp/nomad/plugins"
	"nomad-driver-lima/internal/driver"
	"os"
	"time"
)

func main() {
	if len(os.Args) > 1 && (os.Args[1] == "recover-absent-create" || os.Args[1] == "purge-retained") {
		f := flag.NewFlagSet(os.Args[1], flag.ExitOnError)
		var o driver.FailedCreateOptions
		var operatorStdio, confirmPurge bool
		f.BoolVar(&confirmPurge, "confirm-purge", false, "explicitly approve deletion of exactly this stopped retained instance")
		f.StringVar(&o.ExpectedLedgerSHA256, "ledger-sha256", "", "exact approved original retained ledger SHA256")
		f.BoolVar(&o.AllowGC404, "allow-gc-404", false, "allow verified allocation GC only with matching empty node proof and ledger SHA")
		f.StringVar(&o.Instance, "instance", "", "exact retained instance name")
		f.BoolVar(&operatorStdio, "operator-stdio", false, "two fresh operator observations over the existing SSH controller")
		f.StringVar(&o.StateDir, "state-dir", "", "existing driver state directory")
		f.StringVar(&o.LimaHome, "lima-home", "", "existing Lima home")
		f.StringVar(&o.LimaBinary, "limactl", "", "absolute official limactl binary")
		f.StringVar(&o.RuntimeUser, "runtime-user", "", "existing unprivileged runtime account")
		f.StringVar(&o.NomadBinary, "nomad", "", "absolute official Nomad CLI binary")
		f.StringVar(&o.Key, "key", "", "exact failed ledger key")
		f.StringVar(&o.AllocationID, "allocation", "", "exact failed allocation UUID")
		f.StringVar(&o.NodeID, "node", "", "expected node UUID")
		f.StringVar(&o.JobID, "job", "", "expected job ID")
		f.StringVar(&o.Namespace, "namespace", "", "expected namespace")
		f.StringVar(&o.FailureLog, "failure-log", "", "existing private create diagnostic")
		f.Parse(os.Args[2:])
		if f.NArg() != 0 {
			fmt.Fprintln(os.Stderr, "unexpected recovery arguments")
			os.Exit(2)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		var result driver.FailedCreateResult
		var e error
		if operatorStdio {
			if o.NomadBinary != "" {
				fmt.Fprintln(os.Stderr, "operator-stdio excludes local Nomad CLI")
				os.Exit(2)
			}
			if os.Args[1] == "purge-retained" {
				if !confirmPurge || o.Instance == "" {
					fmt.Fprintln(os.Stderr, "exact instance and --confirm-purge required")
					os.Exit(2)
				}
				result, e = driver.PurgeRetainedStdio(ctx, o, os.Stdin, os.Stdout)
			} else {
				result, e = driver.RecoverFailedCreateStdio(ctx, o, os.Stdin, os.Stdout)
			}
		} else {
			if os.Args[1] == "purge-retained" {
				if !confirmPurge || o.Instance == "" {
					fmt.Fprintln(os.Stderr, "exact instance and --confirm-purge required")
					os.Exit(2)
				}
				result, e = driver.PurgeRetained(ctx, o)
			} else {
				result, e = driver.RecoverFailedCreate(ctx, o)
			}
		}
		if e != nil {
			fmt.Fprintln(os.Stderr, e)
			os.Exit(1)
		}
		json.NewEncoder(os.Stdout).Encode(result)
		return
	}

	plugins.ServeCtx(func(ctx context.Context, l log.Logger) any { return driver.New(ctx, l) })
}
