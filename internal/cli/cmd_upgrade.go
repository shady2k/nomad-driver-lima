package cli

import (
	"context"
	"fmt"
	"strings"
)

// cmdUpgrade moves a box to a newer platform bundle.
//
// The bundle is a field of the job, so the move is a re-registration: the box
// restarts with the new /opt/box mounted, and its disk -- the work -- is kept.
// A box created before there were bundles has none; upgrading it onto the
// newest one is how it migrates.
func (a *App) cmdUpgrade(ctx context.Context, args []string) error {
	fs := newFlagSet("upgrade")
	platformVersion := fs.String("platform", "", "platform bundle version to mount "+
		"(default: the newest published for the box's architecture)")
	timeout := fs.Duration("timeout", defaultWaitTimeout, "how long to wait for the box to come back")
	wait := fs.Bool("wait", true, "wait until the box is running again")
	if err := parseFlags(fs, args, 1); err != nil {
		return err
	}
	id := fs.Arg(0)
	box, job, err := a.findBox(ctx, id)
	if err != nil {
		return err
	}
	if box.State == "orphan" || job == nil {
		return orphanCannot(id, "upgrade")
	}
	if box.JobMissing {
		return stoppedCannot(id, "box upgrade "+id)
	}
	spec, err := SpecFromJob(job)
	if err != nil {
		return err
	}
	// The index decides twice: which image the box runs (does it take a bundle
	// at all?) and which bundles are published for its architecture. A box
	// cannot be upgraded without it, so a missing index is an error here.
	indexURL, err := a.requireImageIndexURL()
	if err != nil {
		return err
	}
	index, err := LoadImageIndex(ctx, a.Images, indexURL)
	if err != nil {
		return err
	}
	image, err := index.Resolve(spec.ImageVersion, spec.Arch)
	if err != nil {
		return err
	}
	if image.Platform != "mount" {
		return usagef("box upgrade %s: the image %s does not take a platform bundle -- it runs the boxd it carries -- "+
			"so it cannot be upgraded to one; the image has to be rebuilt for bundles first", id, spec.ImageVersion)
	}
	want := strings.TrimSpace(*platformVersion)
	if want == "" {
		want = "latest"
	}
	target, err := index.ResolvePlatform(want, spec.Arch)
	if err != nil {
		return err
	}
	if err := target.Validate(); err != nil {
		return err
	}

	out := lifecycleOutput{Action: "upgrade", Box: box, Volume: BoxVolumeName(id)}
	if target.SHA256 == spec.PlatformSHA256 {
		out.Warn = fmt.Sprintf("nothing to change: the box already mounts platform bundle %s", target.Version)
		return a.printLifecycle(&out)
	}
	old := spec.PlatformVersion
	if old == "" {
		old = "none"
	}
	out.Changes = append(out.Changes, fmt.Sprintf("platform %s -> %s", old, target.Version))
	spec.PlatformVersion = target.Version
	spec.PlatformURL = target.URL
	spec.PlatformSHA256 = target.SHA256
	a.refreshCPUReservation(ctx, &spec, box.NodeID)

	// A re-registration is also the moment to pick up the credentials that were
	// staged since the box was created, exactly as a start or a resize does.
	spec.ExecutorItems = a.executorItems(ctx)
	spec.VaultRole = a.vaultRoleFor(spec.VaultRole)
	a.noteNoVaultRole(spec)
	engineered, err := RenderJob(spec)
	if err != nil {
		return err
	}
	resp, err := a.Nomad.RegisterJob(ctx, engineered)
	if err != nil {
		return wrapf(err, "Nomad rejected the platform bundle %s for box %s%s",
			target.Version, id, registerDenied(err, BoxVolumeName(id)))
	}
	evalID := ""
	if resp != nil {
		evalID = resp.EvalID
	}
	if err := a.storeBoxSpec(ctx, id, engineered); err != nil {
		return wrapKeep(err, "the box %s is restarting on platform bundle %s, but its description was not stored, "+
			"so `box start %s` would bring it back on the old one once Nomad collects its job", id, target.Version, id)
	}
	if !*wait {
		out.Warn = fmt.Sprintf("the box is starting on platform bundle %s; `box status %s` shows when it is back",
			target.Version, id)
		return a.printLifecycle(&out)
	}
	rec, err := a.waitForBoxRunning(ctx, id, evalID, *timeout)
	if err != nil {
		out.Box = rec
		return err
	}
	out.Box = rec
	out.Warn = "the box restarted to mount the new bundle; its disk and its sessions' transcripts are kept"
	return a.printLifecycle(&out)
}
