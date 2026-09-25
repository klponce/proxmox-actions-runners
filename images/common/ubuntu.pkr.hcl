# The pins every image build shares: the Packer QEMU plugin and the Ubuntu 26.04 cloud image. Each image directory
# links to this file, so updating the base image is one change.

packer {
  required_plugins {
    qemu = {
      source  = "github.com/hashicorp/qemu"
      version = "= 1.1.6"
    }
  }
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
