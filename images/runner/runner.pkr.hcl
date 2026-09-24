# Runner template image: Ubuntu 26.04 with the QEMU guest agent, the GitHub Actions runner, the units that run one
# job per VM, and a minimal toolset (Docker, git, build tools). Like ARC's runner image, it is lean on purpose:
# workflows bring their own toolchains with setup-* actions. The installer imports it into Proxmox as the template
# workers are cloned from. Built in CI and published as par-runner-<version>.qcow2 with a manifest,
# par-runner-<version>.json, that records the runner version and build time the installer tags the template with.
#
#   packer init images/runner
#   packer build -var version=dev images/runner

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

variable "runner_version" {
  type        = string
  default     = "2.337.0"
  description = "actions/runner release to install. Update together with runner_sha256, within 30 days of a release."
}

variable "runner_sha256" {
  type        = string
  default     = "70920811a4f8ad4328818682bca5c6469c1c942fab52448868071d0063816613"
  description = "SHA-256 of actions-runner-linux-x64-<runner_version>.tar.gz."
}

variable "output_directory" {
  type    = string
  default = "output-runner"
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
  name           = "par-runner-${var.version}"
}

source "qemu" "runner" {
  iso_url      = "https://cloud-images.ubuntu.com/releases/26.04/${var.ubuntu_release}/ubuntu-26.04-server-cloudimg-amd64.img"
  iso_checksum = "sha256:${var.ubuntu_image_sha256}"
  disk_image   = true

  # The template's disk. Every worker's disk is this plus freeDiskGiB, so keep it just big enough for the image.
  disk_size          = "10G"
  format             = "qcow2"
  disk_interface     = "virtio-scsi"
  disk_discard       = "unmap"
  disk_detect_zeroes = "unmap"
  disk_compression   = true

  accelerator = var.accelerator
  cpus        = 2
  memory      = 4096
  headless    = true

  # cloud-init NoCloud seed for the build only: it creates the build user. cleanup.sh resets cloud-init so the
  # image picks up Proxmox's cloud-init drive on its first real boot.
  cd_label = "cidata"
  cd_content = {
    "meta-data" = "instance-id: par-runner-build\nlocal-hostname: par-runner\n"
    "user-data" = templatefile("${local.dir}/user-data.pkrtpl", {
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
  sources = ["source.qemu.runner"]

  provisioner "shell" {
    # env(1), because sudo drops the caller's environment.
    execute_command = "sudo env {{ .Vars }} bash '{{ .Path }}'"
    environment_vars = [
      "PAR_RUNNER_VERSION=${var.runner_version}",
      "PAR_RUNNER_SHA256=${var.runner_sha256}",
    ]
    scripts = [
      "${local.dir}/scripts/base.sh",
      "${local.dir}/scripts/10-runner.sh",
      "${local.dir}/scripts/20-par-runner.sh",
      "${local.dir}/scripts/30-minimal-tools.sh",
      # Running the scripts twice proves they are idempotent.
      "${local.dir}/scripts/10-runner.sh",
      "${local.dir}/scripts/20-par-runner.sh",
      "${local.dir}/scripts/30-minimal-tools.sh",
      # Check the result, run the one-job flow with a stand-in runner, and check that it left nothing behind.
      "${local.dir}/tests/verify.sh",
      "${local.dir}/tests/verify-jit.sh",
      "${local.dir}/tests/verify.sh",
      "${local.dir}/../common/cleanup.sh",
    ]
  }

  post-processor "checksum" {
    checksum_types = ["sha256"]
    output         = "${var.output_directory}/${local.name}.qcow2.sha256"
  }

  # What the installer tags the template with: par-rv-<runner_version> and par-tv-<build_time>.
  post-processor "manifest" {
    output     = "${var.output_directory}/${local.name}.json"
    strip_path = true
    custom_data = {
      runner_version = var.runner_version
    }
  }
}
