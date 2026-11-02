# Nomad *client* configuration for the box driver.
#
# The integration harness (scripts/integration.sh) fills in the __PLACEHOLDERS__
# and starts an agent with this file, so this is the configuration that is
# actually exercised by the tests.

data_dir   = "__DATA_DIR__"
plugin_dir = "__PLUGIN_DIR__"

bind_addr = "127.0.0.1"
advertise {
  http = "127.0.0.1"
  rpc  = "127.0.0.1"
  serf = "127.0.0.1"
}

ports {
  http = __HTTP_PORT__
  rpc  = __RPC_PORT__
  serf = __SERF_PORT__
}

client {
  enabled = true
  servers = ["127.0.0.1:__SERVER_RPC_PORT__"]

  # The box-volume plugin creates one directory per box volume below this
  # directory; the box disk file lives inside that directory.
  host_volumes_dir       = "__VOLUMES_DIR__"
  host_volume_plugin_dir = "__DHV_PLUGIN_DIR__"

  options {
    # Third-party drivers must be allowlisted; box is the only workload driver
    # this node needs.
    "driver.allowlist" = "box"
  }
}

# External plugins are matched to this block by the *executable name*, so the
# driver binary must be installed as "box" (not "box-driver") in plugin_dir.
plugin "box" {
  config {
    limactl             = "__LIMACTL__"
    lima_home           = "__LIMA_HOME__"
    image_cache_dir     = "__IMAGE_CACHE__"
    memory_overhead_mib = 150
    vm_type             = "qemu"

    # The unprivileged user every Lima process runs as. Lima refuses to run as
    # root, so a Nomad client that runs as root (the normal Linux node) must name
    # one: it owns the Lima home and the image cache, and limactl, qemu and the
    # memory scope run with its uid, its groups (kvm) and its own HOME. A client
    # that runs as another user leaves it empty or names that same user.
    runtime_user        = "__RUNTIME_USER__"
  }
}
