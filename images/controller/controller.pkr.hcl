# Controller VM image: Ubuntu 26.04 with the QEMU guest agent, unattended-upgrades, the parcon binary, the parcon
# user, and the parcon.service unit from deploy/. The installer imports it into the par-system pool, writes the
# config and secrets through the guest agent, and enables the unit. Built in CI and published as
# parcon-<version>.qcow2. The plugin and Ubuntu image pins are in ubuntu.pkr.hcl, a link to
# images/common/ubuntu.pkr.hcl. Build parcon first, statically:
#
#   CGO_ENABLED=0 go build -ldflags "-X main.version=dev" -o bin/parcon ./cmd/parcon
#   packer init images/controller
#   packer build -var version=dev -var parcon_binary=bin/parcon images/controller

variable "version" {
  type        = string
  default     = "dev"
  description = "Release version, used in the output file names."
}

variable "parcon_binary" {
  type        = string
  description = "Path of a statically linked parcon binary (CGO_ENABLED=0) to install as /usr/local/bin/parcon."
}

variable "output_directory" {
  type    = string
  default = "output-controller"
}

locals {
  dir = abspath(path.root)
  # The build VM is local, reachable only through QEMU's user-mode network, and its build user is locked by
  # cleanup.sh and deleted on first boot, so a fixed password is fine.
  build_user     = "packer"
  build_password = "packer"
  name           = "parcon-${var.version}"
}

source "qemu" "controller" {
  iso_url      = "https://cloud-images.ubuntu.com/releases/26.04/${var.ubuntu_release}/ubuntu-26.04-server-cloudimg-amd64.img"
  iso_checksum = "sha256:${var.ubuntu_image_sha256}"
  disk_image   = true

  # The installer grows it to the controller VM's 20 GiB.
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
    "meta-data" = "instance-id: parcon-build\nlocal-hostname: parcon\n"
    "user-data" = templatefile("${local.dir}/../common/user-data.pkrtpl", {
      user     = local.build_user
      password = local.build_password
    })
  }

  ssh_username     = local.build_user
  ssh_password     = local.build_password
  ssh_timeout      = "5m"
  shutdown_command = "sudo shutdown -P now"

  output_directory = var.output_directory
  vm_name          = "${local.name}.qcow2"
}

build {
  sources = ["source.qemu.controller"]

  provisioner "file" {
    source      = var.parcon_binary
    destination = "/tmp/parcon"
  }

  provisioner "file" {
    source      = "${local.dir}/../../deploy/parcon.service"
    destination = "/tmp/parcon.service"
  }

  provisioner "shell" {
    execute_command = "sudo bash '{{ .Path }}'"
    scripts = [
      "${local.dir}/../common/base.sh",
      "${local.dir}/scripts/controller.sh",
      # Running the script twice proves it is idempotent.
      "${local.dir}/scripts/controller.sh",
      "${local.dir}/tests/verify.sh",
      "${local.dir}/../common/cleanup.sh",
    ]
  }

  post-processor "checksum" {
    checksum_types = ["sha256"]
    output         = "${var.output_directory}/${local.name}.qcow2.sha256"
  }

  post-processor "manifest" {
    output     = "${var.output_directory}/${local.name}.json"
    strip_path = true
  }
}
