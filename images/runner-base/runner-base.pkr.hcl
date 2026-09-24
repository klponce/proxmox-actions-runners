# Runner base image: Ubuntu 26.04 with the QEMU guest agent and the runner user, and nothing else. The installer
# imports it into Proxmox, and `parcon template build` turns it into a runner template by running the scripts in
# images/ubuntu-26.04/ through the guest agent. Built in CI and published as par-runner-base-<version>.qcow2.
#
#   packer init images/runner-base
#   packer build -var version=dev images/runner-base

packer {
  required_plugins {
    qemu = {
      source  = "github.com/hashicorp/qemu"
      version = "= 1.1.6"
    }
  }
}

variable "version" {
  type        = string
  default     = "dev"
  description = "Release version, used in the output file name."
}

variable "ubuntu_release" {
  type        = string
  default     = "release-20260918"
  description = "Dated Ubuntu 26.04 cloud image release. Update together with ubuntu_image_sha256."
}

variable "ubuntu_image_sha256" {
  type    = string
  default = "4908fb59ccd4e87ae4e8e973b7ef56f535448eacb24a87fd787270c0048987bc"
}

variable "output_directory" {
  type    = string
  default = "output-runner-base"
}

variable "accelerator" {
  type        = string
  default     = "kvm"
  description = "QEMU accelerator; use tcg where KVM isn't available (much slower)."
}

locals {
  # The build VM is local, reachable only through QEMU's user-mode network, and its build user is locked by
  # cleanup.sh and deleted on first boot, so a fixed password is fine.
  build_user     = "packer"
  build_password = "packer"
}

source "qemu" "runner-base" {
  iso_url      = "https://cloud-images.ubuntu.com/releases/26.04/${var.ubuntu_release}/ubuntu-26.04-server-cloudimg-amd64.img"
  iso_checksum = "sha256:${var.ubuntu_image_sha256}"
  disk_image   = true

  # Kept small: the template build grows the disk to fit the toolset, and workers grow it by freeDiskGiB.
  disk_size          = "8G"
  format             = "qcow2"
  disk_interface     = "virtio-scsi"
  disk_discard       = "unmap"
  disk_detect_zeroes = "unmap"
  disk_compression   = true

  accelerator = var.accelerator
  cpus        = 2
  memory      = 2048
  headless    = true

  # cloud-init NoCloud seed for the build only: it creates the build user. cleanup.sh resets cloud-init so the
  # image picks up Proxmox's cloud-init drive on its first real boot.
  cd_label = "cidata"
  cd_content = {
    "meta-data" = "instance-id: par-runner-base-build\nlocal-hostname: par-runner-base\n"
    "user-data" = templatefile("${abspath(path.root)}/user-data.pkrtpl", {
      user     = local.build_user
      password = local.build_password
    })
  }

  ssh_username     = local.build_user
  ssh_password     = local.build_password
  ssh_timeout      = "5m"
  shutdown_command = "sudo shutdown -P now"

  output_directory = var.output_directory
  vm_name          = "par-runner-base-${var.version}.qcow2"
}

build {
  sources = ["source.qemu.runner-base"]

  provisioner "shell" {
    execute_command = "sudo bash '{{ .Path }}'"
    scripts = [
      "${abspath(path.root)}/scripts/base.sh",
      "${abspath(path.root)}/../common/cleanup.sh",
    ]
  }

  post-processor "checksum" {
    checksum_types = ["sha256"]
    output         = "${var.output_directory}/par-runner-base-${var.version}.qcow2.sha256"
  }
}
