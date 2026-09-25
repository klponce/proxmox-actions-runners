# Controller VM image: Ubuntu 26.04 with the QEMU guest agent, unattended-upgrades, the parcon binary, the parcon
# user, and the parcon.service unit from deploy/. The installer imports it into the par-system pool, writes the
# config and secrets through the guest agent, and enables the unit. Built in CI and published as
# parcon-<version>.qcow2. The plugin and Ubuntu image pins and the shared build VM are in ubuntu.pkr.hcl, a link to
# images/common/ubuntu.pkr.hcl. Build parcon first, statically:
#
#   CGO_ENABLED=0 go build -ldflags "-X main.version=dev" -o bin/parcon ./cmd/parcon
#   packer init images/controller
#   packer build -var version=dev -var parcon_binary=bin/parcon images/controller

variable "parcon_binary" {
  type        = string
  description = "Path of a statically linked parcon binary (CGO_ENABLED=0) to install as /usr/local/bin/parcon."
}

variable "output_directory" {
  type    = string
  default = "output-controller"
}

locals {
  name = "parcon-${var.version}"
}

build {
  source "qemu.ubuntu" {
    name = "controller"
    # The installer grows it to the controller VM's 20 GiB.
    disk_size = "8G"
    memory    = 2048
    cd_content = {
      "meta-data" = "instance-id: parcon-build\nlocal-hostname: parcon\n"
      "user-data" = local.build_user_data
    }
    output_directory = var.output_directory
    vm_name          = "${local.name}.qcow2"
  }

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
      "${local.dir}/../common/auto-updates.sh",
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
