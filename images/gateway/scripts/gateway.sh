#!/usr/bin/env bash
# The gateway: dnsmasq for DHCP and DNS on the worker network, nftables for NAT and the firewall, and
# par-gateway-configure, which renders both from the installer's settings. The image ships configured with the
# default worker subnet; the installer runs par-gateway-configure again with the real settings.
set -euo pipefail

main() {
  export DEBIAN_FRONTEND=noninteractive

  # Proxmox puts a VM's net1 in PCI slot 0x13 (net0 in 0x12), so the worker NIC is always called net1 here, whatever
  # its MAC. The 05- prefix orders this before any netplan link file.
  cat >/etc/systemd/network/05-par-net1.link <<'EOF'
[Match]
Path=pci-*:13.0

[Link]
Name=net1
EOF
  chmod 0644 /etc/systemd/network/05-par-net1.link

  cat >/etc/sysctl.d/60-par-gateway.conf <<'EOF'
# The gateway routes IPv4 from the worker network and nothing over IPv6.
net.ipv4.ip_forward = 1
net.ipv6.conf.all.forwarding = 0
EOF
  chmod 0644 /etc/sysctl.d/60-par-gateway.conf
  sysctl -p /etc/sysctl.d/60-par-gateway.conf

  install -m 0755 /tmp/par-gateway-configure /usr/local/sbin/par-gateway-configure

  apt-get update
  # Keep dnsmasq from starting with its default config, which listens on port 53 everywhere and clashes with
  # systemd-resolved. par-gateway-configure starts it with ours.
  printf '#!/bin/sh\nexit 101\n' >/usr/sbin/policy-rc.d
  chmod 0755 /usr/sbin/policy-rc.d
  apt-get -y install --no-install-recommends dnsmasq nftables
  rm /usr/sbin/policy-rc.d
  systemctl enable nftables.service dnsmasq.service
  par-gateway-configure </dev/null
}

main "$@"
