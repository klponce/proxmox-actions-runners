# Gateway VM image: Ubuntu 26.04 with the QEMU guest agent, unattended-upgrades, dnsmasq, nftables, and
# par-gateway-configure. The gateway is the worker network's only way out: it gives workers DHCP, DNS, and NAT to the
# internet, and blocks the LAN. The installer imports it into the par-system pool and configures it through the
# guest agent. Built in CI and published as par-gateway-<version>.qcow2. The plugin and Ubuntu image pins are in
# ubuntu.pkr.hcl, a link to images/common/ubuntu.pkr.hcl.
#
#   packer init images/gateway
#   packer build -var version=dev images/gateway

variable "version" {
  type        = string
  default     = "dev"
  description = "Release version, used in the output file names."
}

variable "output_directory" {
  type    = string
  default = "output-gateway"
}

locals {
  dir = abspath(path.root)
  # The build VM is local, reachable only through QEMU's user-mode network, and its build user is locked by
  # cleanup.sh and deleted on first boot, so a fixed password is fine.
  build_user     = "packer"
  build_password = "packer"
  name           = "par-gateway-${var.version}"
}

source "qemu" "gateway" {
  iso_url      = "https://cloud-images.ubuntu.com/releases/26.04/${var.ubuntu_release}/ubuntu-26.04-server-cloudimg-amd64.img"
  iso_checksum = "sha256:${var.ubuntu_image_sha256}"
  disk_image   = true

  # The gateway VM's disk size, so the installer imports it as is.
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
    "meta-data" = "instance-id: par-gateway-build\nlocal-hostname: par-gateway\n"
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
  sources = ["source.qemu.gateway"]

  provisioner "file" {
    source      = "${local.dir}/par-gateway-configure"
    destination = "/tmp/par-gateway-configure"
  }

  provisioner "shell" {
    execute_command = "sudo bash '{{ .Path }}'"
    scripts = [
      "${local.dir}/../common/base.sh",
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
