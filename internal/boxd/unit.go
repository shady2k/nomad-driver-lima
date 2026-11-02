package boxd

import _ "embed"

// UnitFile is the systemd unit installed as /etc/systemd/system/boxd.service.
//
//go:embed boxd.service
var UnitFile string
