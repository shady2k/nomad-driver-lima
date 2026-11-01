# Operator paths and exact installed upstream version are deployment configuration.
# Use a private unprivileged runtime owner; do not copy credentials into this file.
plugin "nomad-driver-lima" {
  config {
    # Required when Nomad runs as root; use an existing operator-provisioned account.
    # runtime_user = "nomad-runtime"
    limactl = "/opt/lima/bin/limactl"
    lima_home = "/var/lib/nomad-lima"
    state_dir = "/var/lib/nomad-lima-driver"
    template_file = "/etc/nomad-lima/ubuntu-pinned.yaml"
    template_sha256 = "REPLACE_WITH_SHA256_OF_REVIEWED_TEMPLATE"
    expected_version = "2.2.1"
    memory_overhead_mib = 128
    disk_free_floor_mib = 2048
  }
}
