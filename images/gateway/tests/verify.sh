#!/usr/bin/env bash
# Checks the gateway image before cleanup, then leaves it configured with the defaults. Run as root by the image
# build (gateway.pkr.hcl). Real DHCP, NAT, and blocking need a worker network, so they are tested on a live node.
set -euo pipefail

failures=0
check() {
  local what=$1
  shift
  if "$@" >/dev/null 2>&1; then
    echo "ok    $what"
  else
    echo "FAIL  $what"
    failures=$((failures + 1))
  fi
}

RENDERED=(/etc/par-gateway/config /etc/systemd/network/10-par-net1.network /etc/dnsmasq.d/par-gateway.conf
  /etc/nftables.conf)

main() {
  local settings=$'WORKER_SUBNET=10.252.0.0/24\nBLOCK=192.0.2.0/24 198.51.100.7 192.168.1.0/24'
  local first second
  par-gateway-configure <<<"$settings"
  first=$(cat "${RENDERED[@]}" | sha256sum)
  par-gateway-configure <<<"$settings"
  second=$(cat "${RENDERED[@]}" | sha256sum)
  check "a second run renders the same config" test "$first" = "$second"
  check "the settings are saved" grep -qx 'WORKER_SUBNET=10.252.0.0/24' /etc/par-gateway/config
  check "net1 gets the subnet's first address" grep -qx 'Address=10.252.0.1/24' \
    /etc/systemd/network/10-par-net1.network
  check "DHCP hands out the rest of the subnet" grep -qx 'dhcp-range=10.252.0.2,10.252.0.254,255.255.255.0,1h' \
    /etc/dnsmasq.d/par-gateway.conf
  check "the nftables rules are valid" nft -c -f /etc/nftables.conf
  check "the dnsmasq config is valid" dnsmasq --test --conf-dir=/etc/dnsmasq.d
  check "the rules are loaded" nft list table inet par-gateway
  check "BLOCK ranges are blocked" bash -c 'nft list set inet par-gateway blocked | grep -q 198.51.100.7'
  check "private ranges are blocked" bash -c 'nft list set inet par-gateway blocked | grep -q 172.16.0.0/12'
  check "an invalid subnet is refused" bash -c '! par-gateway-configure <<<"WORKER_SUBNET=10.252.0.1/24"'
  check "an invalid BLOCK entry is refused" bash -c '! par-gateway-configure <<<"BLOCK=192.0.2.0/24;"'
  check "an unknown setting is refused" bash -c '! par-gateway-configure <<<"SUBNET=10.252.0.0/24"'
  check "a refused run keeps the old settings" grep -qx 'WORKER_SUBNET=10.252.0.0/24' /etc/par-gateway/config

  # Ship the defaults: the installer configures the real settings.
  par-gateway-configure </dev/null
  check "the defaults use 10.251.0.0/22" grep -qx 'dhcp-range=10.251.0.2,10.251.3.254,255.255.252.0,1h' \
    /etc/dnsmasq.d/par-gateway.conf
  check "IPv4 forwarding is on" test "$(sysctl -n net.ipv4.ip_forward)" = 1
  check "IPv6 forwarding is off" test "$(sysctl -n net.ipv6.conf.all.forwarding)" = 0
  check "dnsmasq is running" systemctl is-active dnsmasq
  check "nftables is enabled" systemctl is-enabled nftables
  check "dnsmasq is enabled" systemctl is-enabled dnsmasq
  check "qemu-guest-agent is enabled" systemctl is-enabled qemu-guest-agent
  check "unattended-upgrades is enabled" systemctl is-enabled unattended-upgrades
  check "automatic updates are on" grep -q 'Unattended-Upgrade "1"' /etc/apt/apt.conf.d/20auto-upgrades
  check "systemd-resolved still answers" resolvectl query ubuntu.com

  if ((failures > 0)); then
    echo "$failures check(s) failed"
    exit 1
  fi
}

main "$@"
