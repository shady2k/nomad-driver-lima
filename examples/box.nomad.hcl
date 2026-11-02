# The box job: one Lima VM per allocation, with its disk on the "boxdisk"
# dynamic host volume so the scheduler pins the box to the node holding the disk.
#
# Placeholders (__...__) are filled in by the integration test so that the same
# file describes the test job and can be copied as a starting point.

job "box" {
  type = "service"

  group "box" {
    # A client restart must not create a replacement allocation: Nomad calls
    # RecoverTask and the driver re-attaches to the running VM.
    disconnect {
      lost_after = "10m"
      replace    = false
      reconcile  = "keep_original"
    }

    # If the guest dies (or the driver reports the VM as gone) the task is
    # restarted in the same allocation, which is where the driver adopts the VM.
    restart {
      attempts = 3
      delay    = "3s"
      interval = "10m"
      mode     = "fail"
    }

    volume "disk" {
      type      = "host"
      source    = "boxdisk"
      read_only = false
    }

    task "box" {
      driver = "box"

      config {
        box_id = "__BOX_ID__"

        image {
          url    = "__IMAGE_URL__"
          sha256 = "__IMAGE_SHA256__"
        }

        cpus       = __CPUS__
        memory_mib = __MEMORY_MIB__
        disk_gib   = __DISK_GIB__

        # Streamed into the Nomad task log, visible with `nomad alloc logs`.
        guest_log_command = __LOG_COMMAND__

        # Rendered task files (local/ and secrets/) are copied here after boot:
        # local/ lands in /etc/box/local on the box disk, and the executor
        # credentials land in /run/box/secrets on the guest's tmpfs, root-only,
        # never on the disk (the driver refuses a secrets path outside /run).
        files_dir_in_guest = "/etc/box"
      }

      volume_mount {
        volume      = "disk"
        destination = "/disk"
      }

      resources {
        cpu    = 2000
        memory = 2048
      }
    }
  }
}
