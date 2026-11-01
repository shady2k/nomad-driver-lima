# Render job label with prepare-worker.py; no model/provider secret in this file.
variable "namespace" { type = string }
variable "datacenter" { type = string }
variable "node_id" { type = string }
variable "owner_id" { type = string }
variable "cpu_mhz" { type = number }
variable "memory_mib" { type = number }
variable "disk_gib" { type = number }
job "__JOB_ID__" {
  namespace = var.namespace
  datacenters = [var.datacenter]
  type = "batch"
  meta { nomad_workers_owner = var.owner_id }
  constraint {
    attribute = "${node.unique.id}"
    value = var.node_id
  }
  constraint {
    attribute = "${attr.driver.lima.guest.arch}"
    value = "amd64"
  }
  group "worker" {
    count = 1
    ephemeral_disk { size = var.disk_gib * 1024 + 1024 }
    restart {
      attempts = 0
      mode = "fail"
    }
    reschedule {
      attempts = 0
      unlimited = false
    }
    task "worker" {
      driver = "lima"
      config {
        guest_os = "linux"
        architecture = "amd64"
        guest_cpus = 1
        memory_mib = var.memory_mib
        disk_gib = var.disk_gib
        command = ["/bin/sh", "/opt/nomad-workers/start-worker.sh"]
      }
      resources {
        cpu = var.cpu_mhz
        memory = var.memory_mib + 128
      }
    }
  }
}
