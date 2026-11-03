// Command box is the coordinator's only tool: it runs boxes on a Nomad cluster
// through the Nomad API and keeps no local state.
//
// Nomad is the source of truth. Everything this command knows about a box it
// reads back from Nomad: the job box-<id>, its dynamic host volume and its
// allocation. Every guest call is `nomad alloc exec` of /usr/local/bin/boxctl,
// as described in docs/boxctl-contract.md.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/shady2k/nomad-driver-lima/internal/cli"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	nomad, err := cli.NewNomadClient()
	if err != nil {
		fmt.Fprintf(os.Stderr, "box: %v\n", err)
		os.Exit(cli.ExitError)
	}
	app := cli.New(nomad, &cli.HTTPImageSource{}, nil)
	app.Out = os.Stdout
	app.ErrOut = os.Stderr
	app.In = os.Stdin
	app.Terminal = cli.NewOSTerminal(os.Stdin, os.Stdout)
	app.SetImageIndexURL(os.Getenv("BOX_IMAGE_INDEX"))
	app.SetVaultRole(os.Getenv("BOX_VAULT_ROLE"))
	os.Exit(app.Run(ctx, os.Args[1:]))
}
