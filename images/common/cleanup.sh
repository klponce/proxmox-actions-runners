#!/usr/bin/env bash
# Final step of every image build: remove the machine's identity and build leftovers so each VM made from the image
# gets its own machine ID, SSH host keys, and cloud-init run. Used by the Packer builds (runner base, gateway,
# controller) and by `parcon template build`. Safe to run more than once.
set -euo pipefail

# write_file MODE PATH writes stdin to PATH with MODE, atomically, so a rerun or an interrupted run never leaves a
# partial file. (install from /dev/stdin fails depending on how bash implements the heredoc.)
write_file() {
  local mode=$1 path=$2 tmp
  tmp=$(mktemp "$path.XXXXXX")
  cat >"$tmp"
  chmod "$mode" "$tmp"
  mv -f "$tmp" "$path"
}

main() {
  export DEBIAN_FRONTEND=noninteractive

  apt-get -y autoremove --purge
  apt-get clean
  rm -rf /var/lib/apt/lists/*

  remove_build_user_on_first_boot

  # cloud-init runs again from scratch on the next boot and reads that VM's own cloud-init drive. Drop what the
  # build's run wrote: its network config matches the build VM's MAC, and its SSH settings allow passwords.
  cloud-init clean --logs --seed
  rm -f /etc/netplan/50-cloud-init.yaml /etc/ssh/sshd_config.d/50-cloud-init.conf
  rm -f /etc/ssh/ssh_host_*

  # An empty machine-id makes systemd generate a new one on first boot.
  truncate -s 0 /etc/machine-id
  if [[ -f /var/lib/dbus/machine-id && ! -L /var/lib/dbus/machine-id ]]; then
    rm -f /var/lib/dbus/machine-id
  fi

  rm -rf /tmp/* /var/tmp/*
  journalctl --rotate >/dev/null 2>&1 || true
  journalctl --vacuum-time=1s >/dev/null 2>&1 || true
  find /var/log -type f -exec truncate -s 0 {} +
  rm -f /root/.bash_history /home/*/.bash_history

  # Let the image shrink: discard freed blocks so qemu-img can skip them.
  fstrim -av >/dev/null 2>&1 || true
  sync
}

# A Packer build connects as a temporary user created by its cloud-init seed. Lock it now and delete it on the
# image's first boot, before SSH starts; it can't be deleted while the build is still connected as it.
remove_build_user_on_first_boot() {
  local user="${SUDO_USER:-}"
  if [[ -z $user || $user == root || $user == runner ]]; then
    return
  fi
  passwd -l "$user" >/dev/null
  echo "$user" >/etc/par-build-user

  write_file 0755 /usr/local/sbin/par-remove-build-user <<'EOF'
#!/bin/sh
# Deletes the image build user on first boot. Installed by images/common/cleanup.sh.
user=$(cat /etc/par-build-user) || exit 0
userdel --remove "$user" 2>/dev/null || true
# Drop only the build user's sudo rule; this boot's cloud-init may have written rules for its own users.
sed -i "/^$user /d" /etc/sudoers.d/90-cloud-init-users 2>/dev/null || true
rm -f /etc/par-build-user
systemctl disable par-remove-build-user.service
EOF
  write_file 0644 /etc/systemd/system/par-remove-build-user.service <<'EOF'
[Unit]
Description=Remove the image build user
ConditionPathExists=/etc/par-build-user
# Not before ssh.socket: sockets start before normal units can, and that ordering cycle makes systemd drop
# ssh.socket. A connection accepted early simply waits for ssh.service, which waits for this unit.
Before=ssh.service

[Service]
Type=oneshot
ExecStart=/usr/local/sbin/par-remove-build-user

[Install]
WantedBy=multi-user.target
EOF
  systemctl enable par-remove-build-user.service
}

main "$@"
