# The box disk volume: a Nomad dynamic host volume managed by the box-volume
# plugin. `create` makes the directory, `delete` stops and deletes the Lima
# instance that owns a disk inside it and then removes the directory.
#
# There is no `parameters` block, and that is the point: limactl, the Lima home
# and the unprivileged user that owns the directory are settings of the *node*,
# not of the volume, so whoever creates the volume cannot choose them. The plugin
# reads them from /etc/box/volume.json, which the node role writes, and it refuses
# a request that carries any parameter at all.
#
# __PLACEHOLDERS__ in this file are filled in by the integration harness, which
# also writes the settings file the plugin reads.

type         = "host"
name         = "boxdisk"
plugin_id    = "box-volume"
capacity_min = "1GiB"
capacity_max = "20GiB"

capability {
  access_mode     = "single-node-writer"
  attachment_mode = "file-system"
}
