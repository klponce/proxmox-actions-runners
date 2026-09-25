# Gateway VM image: Ubuntu 26.04 with the QEMU guest agent, unattended-upgrades, dnsmasq, nftables, and
# par-gateway-configure. The gateway is the worker network's only way out: it gives workers DHCP, DNS, and NAT to the
# internet, and blocks the LAN. The installer imports it into the par-system pool and configures it through the
# guest agent. Built in CI and published as par-gateway-<version>.qcow2. The plugin and Ubuntu image pins and the
# shared build VM are in ubuntu.pkr.hcl, a link to images/common/ubuntu.pkr.hcl.
#
#   packer init images/gateway
#   packer build -var version=dev images/gateway

variable "output_directory" {
  type    = string
  default = "output-gateway"
}

locals {
  name = "par-gateway-${var.version}"
}

build {
  source "qemu.ubuntu" {
    name = "gateway"
    # The gateway VM's disk size, so the installer imports it as is.
    disk_size = "8G"
    memory    = 2048
    cd_content = {
      "meta-data" = "instance-id: par-gateway-build\nlocal-hostname: par-gateway\n"
      "user-data" = local.build_user_data
    }
    output_directory = var.output_directory
    vm_name          = "${local.name}.qcow2"
  }

  provisioner "file" {
    source      = "${local.dir}/par-gateway-configure"
    destination = "/tmp/par-gateway-configure"
  }

  provisioner "shell" {
    execute_command = "sudo bash '{{ .Path }}'"
    scripts = [
      "${local.dir}/../common/base.sh",
      "${local.dir}/../common/auto-updates.sh",
      "${local.dir}/scripts/gateway.sh",
      # Running the script twice proves it is idempotent.
      "${local.dir}/scripts/gateway.sh",
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
