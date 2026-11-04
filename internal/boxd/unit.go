package boxd

import _ "embed"

// UnitFile is the systemd unit installed as /etc/systemd/system/boxd.service
// for a box that mounts the platform bundle: the daemon runs out of the bundle
// at /opt/box/bin, and the executors are found on the bundle's bin first.
//
//go:embed boxd.service
var UnitFile string

// InstalledUnitFile is the systemd unit installed as /etc/systemd/system/boxd.service
// for an image that carries boxd itself and mounts no bundle: the daemon runs
// from /usr/local/bin and the image's own executors.
//
//go:embed boxd-installed.service
var InstalledUnitFile string
