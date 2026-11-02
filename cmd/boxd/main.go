// Command boxd is the box guest daemon; the same binary is the control client
// boxctl (by argv[0], or by the "boxctl" subcommand).
//
// In the guest, systemd runs it as boxd.service and the Nomad driver execs
// /usr/local/bin/boxctl ... inside the box task.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/shady2k/nomad-driver-lima/internal/boxd"
	"github.com/shady2k/nomad-driver-lima/internal/version"
)

func main() {
	log.SetFlags(log.LstdFlags | log.LUTC)
	log.SetPrefix("boxd: ")

	name := filepath.Base(os.Args[0])
	args := os.Args[1:]

	if name == "boxctl" {
		os.Exit(boxd.RunBoxctl(args, os.Stdin, os.Stdout, os.Stderr))
	}
	if len(args) > 0 && args[0] == "boxctl" {
		os.Exit(boxd.RunBoxctl(args[1:], os.Stdin, os.Stdout, os.Stderr))
	}
	if len(args) > 0 && args[0] == "daemon" {
		args = args[1:]
	}
	os.Exit(runDaemon(args))
}

func runDaemon(args []string) int {
	fs := flag.NewFlagSet("boxd", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	socket := fs.String("socket", envOr("BOXD_SOCKET", boxd.DefaultSocket), "unix socket to listen on")
	stateDir := fs.String("state-dir", envOr("BOXD_STATE_DIR", "/var/lib/boxd"), "durable state directory (sessions and transcripts)")
	runtimeDir := fs.String("runtime-dir", envOr("BOXD_RUNTIME_DIR", "/run/boxd"), "executor configuration for this boot (a tmpfs)")
	secretsDir := fs.String("secrets-dir", envOr("BOXD_SECRETS_DIR", "/run/box/secrets"), "executor credentials the driver copied onto the tmpfs (one directory per executor)")
	localDir := fs.String("local-dir", "/etc/box/local", "files the Nomad driver copies into the box")
	primeAgent := fs.String("prime-agent", "prime-agent", "prime-agent binary")
	opencode := fs.String("opencode", "opencode", "opencode binary")
	claudeACP := fs.String("claude-acp", "claude-code-acp", "claude-code-acp binary")
	showVersion := fs.Bool("version", false, "print the version and exit")
	printUnit := fs.Bool("print-unit", false, "print the systemd unit and exit")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *showVersion {
		fmt.Println(version.Version)
		return 0
	}
	if *printUnit {
		fmt.Print(boxd.UnitFile)
		return 0
	}

	cfg := boxd.DaemonConfig{
		Socket:     *socket,
		StateDir:   *stateDir,
		RuntimeDir: *runtimeDir,
		SecretsDir: *secretsDir,
		LocalDir:   *localDir,
		PrimeAgent: *primeAgent,
		OpenCode:   *opencode,
		ClaudeACP:  *claudeACP,
		// Events go to stdout as JSON lines: under systemd that is the journal,
		// which is exactly what the driver streams into the Nomad task log.
		Journal: os.Stdout,
		Logf:    log.Printf,
	}
	d, err := boxd.NewDaemon(cfg)
	if err != nil {
		log.Printf("start: %v", err)
		return 1
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := d.Serve(ctx); err != nil {
		log.Printf("serve: %v", err)
		return 1
	}
	return 0
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
