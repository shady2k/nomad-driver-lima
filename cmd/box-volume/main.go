// Command box-volume is the external dynamic host volume plugin for the box
// task driver. Nomad runs it as `box-volume fingerprint|create|delete` and
// reads one JSON object from stdout.
package main

import (
	"os"

	"github.com/shady2k/nomad-driver-lima/internal/volume"
)

func main() {
	os.Exit(volume.Run(os.Args[1:], os.Getenv, os.Stdout))
}
