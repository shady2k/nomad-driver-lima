variable "architecture" { type = string }
variable "cpu_mhz_per_vcpu" { type = number }
variable "disk_gib" { type = number }
job "lima-synthetic" {
  type = "batch"
  datacenters = ["dc1"]
  constraint {
    attribute = "${attr.driver.lima.guest.arch}"
    value = var.architecture
  }
  group "work" {
    ephemeral_disk { size = var.disk_gib * 1024 + 1024 }
    restart {
      attempts = 0
      mode = "fail"
    }
    reschedule {
      attempts = 0
      unlimited = false
    }
    task "work" {
      driver = "lima"
      config {
        guest_os = "linux"
        architecture = var.architecture
        guest_cpus = 1
        memory_mib = 1024
        disk_gib = var.disk_gib
        command = ["/usr/bin/python3", "-c", "print('synthetic guest workload')"]
      }
      resources {
        cpu = var.cpu_mhz_per_vcpu
        memory = 1152
      }
    }
  }
}
