# Local test of the template scripts, without Proxmox: boots the runner base image in QEMU, runs the same scripts
# `parcon template build` runs through the guest agent, then checks the result. The output is for inspection only.
#
#   packer build -var version=dev images/runner-base
#   packer init images/ubuntu-26.04
#   packer build -var base_image=output-runner-base/par-runner-base-dev.qcow2 images/ubuntu-26.04

packer {
  required_plugins {
    qemu = {
      source  = "github.com/hashicorp/qemu"
      version = "= 1.1.6"
    }
  }
}

variable "base_image" {
  type        = string
  description = "Path of a runner base image built from images/runner-base."
}

variable "output_directory" {
  type    = string
  default = "output-template-test"
}

variable "accelerator" {
  type    = string
  default = "kvm"
}

source "qemu" "template-test" {
  iso_url      = var.base_image
  iso_checksum = "none"
  disk_image   = true

  disk_size          = "16G"
  format             = "qcow2"
  disk_interface     = "virtio-scsi"
  disk_discard       = "unmap"
  disk_detect_zeroes = "unmap"

  accelerator = var.accelerator
  cpus        = 2
  memory      = 4096
  headless    = true

  # The base image deletes its own build user on first boot, so this test brings its own.
  cd_label = "cidata"
  cd_content = {
    "meta-data" = "instance-id: par-template-test\nlocal-hostname: par-template-test\n"
    "user-data" = templatefile("${abspath(path.root)}/../runner-base/user-data.pkrtpl", {
      user     = "tester"
      password = "tester"
    })
  }

  ssh_username     = "tester"
  ssh_password     = "tester"
  ssh_timeout      = "5m"
  shutdown_command = "sudo shutdown -P now"

  output_directory = var.output_directory
  vm_name          = "par-template-test.qcow2"
}

build {
  sources = ["source.qemu.template-test"]

  provisioner "shell" {
    execute_command = "sudo bash '{{ .Path }}'"
    scripts = [
      "${abspath(path.root)}/10-runner.sh",
      "${abspath(path.root)}/20-par-runner.sh",
      "${abspath(path.root)}/30-minimal-tools.sh",
      # Running every script twice proves they are idempotent.
      "${abspath(path.root)}/10-runner.sh",
      "${abspath(path.root)}/20-par-runner.sh",
      "${abspath(path.root)}/30-minimal-tools.sh",
      "${abspath(path.root)}/tests/verify.sh",
      "${abspath(path.root)}/tests/verify-jit.sh",
      "${abspath(path.root)}/tests/verify.sh",
      "${abspath(path.root)}/../common/cleanup.sh",
    ]
  }
}
