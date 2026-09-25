# Runner template image: Ubuntu 26.04 with the QEMU guest agent, the GitHub Actions runner, the units that run one
# job per VM, and a minimal toolset (Docker, git, build tools). Like ARC's runner image, it is lean on purpose:
# workflows bring their own toolchains with setup-* actions. The installer imports it into Proxmox as the template
# workers are cloned from. Built in CI and published as par-runner-<version>.qcow2 with a manifest,
# par-runner-<version>.json, that records the runner version and build time the installer tags the template with.
#
# The plugin and Ubuntu image pins and the shared build VM are in ubuntu.pkr.hcl, a link to
# images/common/ubuntu.pkr.hcl.
#
#   packer init images/runner
#   packer build -var version=dev images/runner

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

locals {
  name = "par-runner-${var.version}"
}

build {
  source "qemu.ubuntu" {
    name = "runner"
    # The template's disk. Every worker's disk is this plus freeDiskGiB, so keep it just big enough for the image.
    disk_size = "10G"
    memory    = 4096
    cd_content = {
      "meta-data" = "instance-id: par-runner-build\nlocal-hostname: par-runner\n"
      "user-data" = local.build_user_data
    }
    output_directory = var.output_directory
    vm_name          = "${local.name}.qcow2"
  }

  provisioner "shell" {
    # env(1), because sudo drops the caller's environment.
    execute_command = "sudo env {{ .Vars }} bash '{{ .Path }}'"
    environment_vars = [
      "PAR_RUNNER_VERSION=${var.runner_version}",
      "PAR_RUNNER_SHA256=${var.runner_sha256}",
    ]
    scripts = [
      "${local.dir}/../common/base.sh",
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
