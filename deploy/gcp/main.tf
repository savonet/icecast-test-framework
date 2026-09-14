terraform {
  required_providers {
    google = {
      source  = "hashicorp/google"
      version = ">= 6.0"
    }
  }
}

variable "project" { type = string }
variable "zone" {
  type    = string
  default = "us-central1-a"
}
variable "machine_type" {
  type    = string
  default = "c3-standard-8"
}
# The load boxes drive more listeners than the server serves: several of
# them, each with a block of alias addresses to draw source ports from.
variable "load_count" {
  type    = number
  default = 1
}
variable "load_machine_type" {
  type    = string
  default = "c3-standard-8"
}
variable "load_alias_range" {
  type    = string
  default = "/28"
}
variable "spot" {
  type    = bool
  default = true
}
variable "image" {
  type    = string
  default = "debian-cloud/debian-13"
}
variable "liquidsoap_release" {
  type    = string
  default = "rolling-release-v2.5.x"
}

provider "google" {
  project = var.project
  zone    = var.zone
}

locals {
  load_roles = [for i in range(var.load_count) : "load-${i + 1}"]
  roles      = concat(["server"], local.load_roles)
  provision  = replace(file("${path.module}/../provision.sh"), "$${liquidsoap_release}", var.liquidsoap_release)
}

resource "google_compute_instance" "box" {
  for_each     = toset(local.roles)
  name         = "icetest-${each.key}"
  machine_type = each.key == "server" ? var.machine_type : var.load_machine_type
  zone         = var.zone
  tags         = ["icetest"]

  # A shape change is applied by stopping the box in place; a preempted or
  # stopped box is started again by the next apply.
  allow_stopping_for_update = true
  desired_status            = "RUNNING"

  boot_disk {
    initialize_params {
      image = var.image
      size  = 20
      type  = "pd-balanced"
    }
  }

  # An external address gives the box apt and GitHub access without Cloud NAT.
  network_interface {
    network = "default"
    access_config {}
    dynamic "alias_ip_range" {
      for_each = each.key == "server" ? [] : [var.load_alias_range]
      content {
        ip_cidr_range = alias_ip_range.value
      }
    }
  }

  scheduling {
    provisioning_model          = var.spot ? "SPOT" : "STANDARD"
    preemptible                 = var.spot
    automatic_restart           = !var.spot
    instance_termination_action = var.spot ? "STOP" : null
  }

  # Google's Debian images have no cloud-init; the guest agent runs this at boot.
  metadata = {
    startup-script = local.provision
  }
}

output "server_internal_ip" {
  value = google_compute_instance.box["server"].network_interface[0].network_ip
}
output "load_boxes" {
  value = local.load_roles
}
output "load_alias_ranges" {
  value = { for r in local.load_roles : r => google_compute_instance.box[r].network_interface[0].alias_ip_range[0].ip_cidr_range }
}
output "zone" { value = var.zone }
output "project" { value = var.project }
