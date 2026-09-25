# What every image build shares: the Packer QEMU plugin, the Ubuntu 26.04 cloud image, and the build VM that boots
# it. Each image directory links to this file, so updating the base image is one change. Each image's build block
# names the source and sets what differs: disk size, memory, the seed's host name, and the output.

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
  description = "Release version, used in the output file names."
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

variable "accelerator" {
  type        = string
  default     = "kvm"
  description = "QEMU accelerator; use tcg where KVM isn't available (much slower)."
}

locals {
  dir = abspath(path.root)
  # The build VM is local, reachable only through QEMU's user-mode network, and its build user is locked by
  # cleanup.sh and deleted on first boot, so a fixed password is fine.
  build_user     = "packer"
  build_password = "packer"
  # The cloud-init NoCloud seed's user-data, for the build only: it creates the build user. cleanup.sh resets
  # cloud-init so the image picks up Proxmox's cloud-init drive on its first real boot. Each build adds meta-data
  # with its own host name.
  build_user_data = templatefile("${local.dir}/../common/user-data.pkrtpl", {
    user     = local.build_user
    password = local.build_password
  })
}

source "qemu" "ubuntu" {
  iso_url      = "https://cloud-images.ubuntu.com/releases/26.04/${var.ubuntu_release}/ubuntu-26.04-server-cloudimg-amd64.img"
  iso_checksum = "sha256:${var.ubuntu_image_sha256}"
  disk_image   = true

  format             = "qcow2"
  disk_interface     = "virtio-scsi"
  disk_discard       = "unmap"
  disk_detect_zeroes = "unmap"
  disk_compression   = true

  accelerator = var.accelerator
  cpus        = 2
  headless    = true

  cd_label = "cidata"

  ssh_username     = local.build_user
  ssh_password     = local.build_password
  ssh_timeout      = "5m"
  shutdown_command = "sudo shutdown -P now"
}
