#!/usr/bin/env bash
# install.sh installs proxmox-actions-runners on a standalone Proxmox VE 9 node: ephemeral GitHub Actions runners in
# VMs, one job per VM. Run it as root on the node. See docs/install.md for the design.
#
#   bash install.sh [install|upgrade|uninstall|check] [--answers FILE] [--yes] [--dry-run]
#
# It changes the host only through Proxmox's own tools (pveum, pvesh, qm) and creates only objects it can find again
# by name or tag, so `uninstall` removes everything. Secrets never touch the host's disk or a command line.
#
# shellcheck disable=SC2016 # Perl code and commands for the VMs are single-quoted on purpose.
set -euo pipefail

# The release workflow replaces the two placeholders: the version, such as 0.1.0, and the release's SHA256SUMS, one
# "<sha256>  <asset>" line per asset. A copy from the source tree refuses to install.
PAR_VERSION="@PAR_VERSION@"
PAR_SHA256SUMS="@PAR_SHA256SUMS@"

readonly REPO_URL="https://github.com/klponce/proxmox-actions-runners"
readonly HELPER_URL="https://klponce.github.io/proxmox-actions-runners/app/v1/"

# Proxmox objects. Uninstall finds everything by these names and tags.
readonly SYSTEM_POOL=par-system RUNNER_POOL=par-runners
readonly ROLE=PARController PVE_USER=par@pve TOKEN=controller
readonly ZONE=parzone VNET=parnet
readonly TAG_MANAGED=par-managed TAG_GATEWAY=par-gateway TAG_CONTROLLER=par-controller TAG_TEMPLATE=par-template
readonly TAG_BUILD=par-build RELEASE_TAG_PREFIX=par-release-

# The controller VM (docs/install.md, "Controller VM contract").
readonly ETC=/etc/proxmox-actions-runners
readonly KEY_FILE=$ETC/github-app.pem
# A copy of the node's CA, which the controller verifies the API's certificate against.
readonly CA_FILE=$ETC/pve-ca.pem

# Where Proxmox keeps the node's certificates. Tests point it elsewhere.
PVE_DIR=/etc/pve

# The token's role: exactly what internal/proxmox/access.go requires, on the runner pool, the storage, and the VNet.
# A Go test keeps this list in sync with it.
readonly PRIVILEGES=(
	VM.Allocate VM.Clone
	VM.Config.CPU VM.Config.Memory VM.Config.Disk VM.Config.Network VM.Config.Cloudinit VM.Config.Options
	VM.PowerMgmt VM.Audit
	VM.GuestAgent.Audit VM.GuestAgent.FileWrite
	Pool.Audit
	Datastore.AllocateSpace Datastore.Audit
	SDN.Use
)

# ReservedVMIDs in internal/config: the IDs at the end of the range that hold templates and smoke-test clones.
readonly RESERVED_VMIDS=4
# Disk sizes in GiB: the images' disks and the controller VM's, which the installer grows.
readonly TEMPLATE_GIB=10 GATEWAY_GIB=8 CONTROLLER_GIB=20 DOWNLOAD_GIB=4
readonly WORKER_MEMORY_MIB=8192

# Settings: from the answers file, prompts, or defaults. `settings_help` describes them.
readonly SETTINGS=(PAR_GITHUB_URL PAR_GITHUB_APP PAR_GITHUB_APP_CLIENT_ID PAR_SCALE_SET PAR_LABELS PAR_RUNNER_GROUP
	PAR_MIN_RUNNERS PAR_MAX_RUNNERS PAR_STORAGE PAR_BRIDGE PAR_VLAN PAR_PVE_ADDRESS PAR_GATEWAY_IP PAR_CONTROLLER_IP
	PAR_LAN_GATEWAY PAR_WORKER_SUBNET PAR_VMID_START PAR_VMID_END)

COMMAND=install
ANSWERS=""
ASSUME_YES=0
DRY_RUN=0
WORK_DIR=""
WARNINGS=()

# ---------------------------------------------------------------------------------------------------------------
# Output and command helpers

say() { printf '%s\n' "$*"; }
step() { printf '\n==> %s\n' "$*"; }
warn() { printf 'warning: %s\n' "$*" >&2; }
die() {
	printf 'error: %s\n' "$*" >&2
	exit 1
}

# change runs a command that changes the host and logs it first. With --dry-run it only logs it. Secrets are never
# passed as arguments, so the log never holds one.
change() {
	printf '    $ %s\n' "$*"
	((DRY_RUN)) || "$@"
}

# confirm asks a yes/no question on the terminal. --yes answers yes.
confirm() {
	((ASSUME_YES)) && return 0
	[[ -r /dev/tty ]] || die "$1 Run with --yes to answer yes without a terminal."
	local answer
	read -r -p "$1 [y/N] " answer </dev/tty
	[[ $answer == [yY] || $answer == [yY][eE][sS] ]]
}

# json runs a Perl expression over the JSON document on stdin, bound to $d, with the script's arguments in @ARGV.
json() {
	local expr=$1
	shift
	perl -MJSON -e "my \$d = decode_json(do { local \$/; <STDIN> }); $expr" "$@"
}

# ---------------------------------------------------------------------------------------------------------------
# Arguments and settings

usage() {
	cat <<EOF
usage: bash install.sh [command] [--answers FILE] [--yes] [--dry-run]

commands:
  install     install, or upgrade if an install is found (the default)
  upgrade     upgrade an existing install: controller, gateway, and runner template
  uninstall   remove everything the installer and the controller created
  check       run the preflight checks only

options:
  --answers FILE  read settings from a KEY=value file instead of prompting (see below)
  --yes           answer yes to every confirmation
  --dry-run       run the checks, print the plan, and change nothing

$(settings_help)
EOF
}

settings_help() {
	cat <<'EOF'
settings (answers file keys):
  PAR_GITHUB_URL            organization or repository, https://github.com/<org> or https://github.com/<owner>/<repo>
  PAR_GITHUB_APP            manifest (create a new App in the browser, the default) or manual (use an existing App)
  PAR_GITHUB_APP_CLIENT_ID  the existing App's Client ID, for manual
  PAR_SCALE_SET             scale set name, used in runs-on (default proxmox-ubuntu-26.04)
  PAR_LABELS                comma-separated runs-on labels (default: the scale set name)
  PAR_RUNNER_GROUP          GitHub runner group (default default; repositories must use default)
  PAR_MIN_RUNNERS           idle workers to keep booted (default 0)
  PAR_MAX_RUNNERS           most workers at once (default 2)
  PAR_STORAGE               storage for VM disks (default local-lvm)
  PAR_BRIDGE                LAN bridge for the controller and gateway VMs (default vmbr0)
  PAR_VLAN                  VLAN tag on the LAN bridge (default none)
  PAR_PVE_ADDRESS           this node's address the controller uses for the API (default: its address on the bridge)
  PAR_GATEWAY_IP            gateway VM's LAN address: dhcp or a CIDR such as 192.0.2.10/24 (default dhcp)
  PAR_CONTROLLER_IP         controller VM's LAN address: dhcp or a CIDR (default dhcp)
  PAR_LAN_GATEWAY           LAN router, needed with a static address
  PAR_WORKER_SUBNET         worker network (default 10.251.0.0/22)
  PAR_VMID_START            first VMID for workers and templates (default 10000)
  PAR_VMID_END              last VMID (default 10099)
EOF
}

parse_args() {
	while (($# > 0)); do
		case $1 in
		install | upgrade | uninstall | check) COMMAND=$1 ;;
		--answers)
			(($# >= 2)) || die "--answers needs a file"
			ANSWERS=$2
			shift
			;;
		--yes) ASSUME_YES=1 ;;
		--dry-run) DRY_RUN=1 ;;
		-h | --help)
			usage
			exit 0
			;;
		*)
			usage >&2
			exit 2
			;;
		esac
		shift
	done
}

# load_answers reads KEY=value lines. Values are taken literally, never evaluated.
load_answers() {
	local file=$1 line key value n=0
	[[ -r $file ]] || die "can't read the answers file $file"
	while IFS= read -r line || [[ -n $line ]]; do
		n=$((n + 1))
		[[ $line =~ ^[[:space:]]*(#|$) ]] && continue
		[[ $line == *=* ]] || die "$file:$n: not KEY=value"
		key=${line%%=*}
		value=${line#*=}
		value=${value%\"}
		value=${value#\"}
		is_setting "$key" || die "$file:$n: unknown setting $key"
		printf -v "$key" '%s' "$value"
	done <"$file"
}

is_setting() {
	local s
	for s in "${SETTINGS[@]}"; do
		[[ $s == "$1" ]] && return 0
	done
	return 1
}

apply_defaults() {
	: "${PAR_GITHUB_URL:=}"
	: "${PAR_GITHUB_APP:=manifest}"
	: "${PAR_GITHUB_APP_CLIENT_ID:=}"
	: "${PAR_SCALE_SET:=proxmox-ubuntu-26.04}"
	: "${PAR_LABELS:=$PAR_SCALE_SET}"
	: "${PAR_RUNNER_GROUP:=default}"
	: "${PAR_MIN_RUNNERS:=0}"
	: "${PAR_MAX_RUNNERS:=2}"
	: "${PAR_STORAGE:=local-lvm}"
	: "${PAR_BRIDGE:=vmbr0}"
	: "${PAR_VLAN:=}"
	: "${PAR_PVE_ADDRESS:=}"
	: "${PAR_GATEWAY_IP:=dhcp}"
	: "${PAR_CONTROLLER_IP:=dhcp}"
	: "${PAR_LAN_GATEWAY:=}"
	: "${PAR_WORKER_SUBNET:=10.251.0.0/22}"
	: "${PAR_VMID_START:=10000}"
	: "${PAR_VMID_END:=10099}"
}

# prompt_settings asks for the settings that have no default, on the terminal.
prompt_settings() {
	if [[ -z $PAR_GITHUB_URL ]]; then
		[[ -r /dev/tty ]] || die "PAR_GITHUB_URL is required; set it in the answers file"
		read -r -p "GitHub organization or repository URL (https://github.com/<org> or <owner>/<repo>): " \
			PAR_GITHUB_URL </dev/tty
	fi
	PAR_GITHUB_URL=${PAR_GITHUB_URL%/}
	if [[ $PAR_GITHUB_APP == manual && -z $PAR_GITHUB_APP_CLIENT_ID ]]; then
		[[ -r /dev/tty ]] || die "PAR_GITHUB_APP_CLIENT_ID is required with PAR_GITHUB_APP=manual"
		read -r -p "The existing GitHub App's Client ID: " PAR_GITHUB_APP_CLIENT_ID </dev/tty
	fi
}

# validate_settings checks every setting. Values end up in YAML and in Proxmox commands, so each must match a strict
# pattern.
validate_settings() {
	local errors=()
	[[ $PAR_GITHUB_URL =~ ^https://github\.com/[A-Za-z0-9-]+(/[A-Za-z0-9._-]+)?$ ]] ||
		errors+=("PAR_GITHUB_URL must be https://github.com/<org> or https://github.com/<owner>/<repo>")
	[[ $PAR_GITHUB_APP == manifest || $PAR_GITHUB_APP == manual ]] ||
		errors+=("PAR_GITHUB_APP must be manifest or manual")
	[[ $PAR_GITHUB_APP != manual || $PAR_GITHUB_APP_CLIENT_ID =~ ^[A-Za-z0-9._-]+$ ]] ||
		errors+=("PAR_GITHUB_APP_CLIENT_ID must be the App's Client ID")
	[[ $PAR_SCALE_SET =~ ^[A-Za-z0-9._-]+$ ]] || errors+=("PAR_SCALE_SET may contain only letters, digits, . _ -")
	[[ $PAR_LABELS =~ ^[A-Za-z0-9._-]+(,[A-Za-z0-9._-]+)*$ ]] ||
		errors+=("PAR_LABELS must be comma-separated names of letters, digits, . _ -")
	[[ $PAR_RUNNER_GROUP =~ ^[A-Za-z0-9._\ -]+$ ]] || errors+=("PAR_RUNNER_GROUP has invalid characters")
	if is_repository && [[ $PAR_RUNNER_GROUP != default ]]; then
		errors+=("repository runners must use the runner group default")
	fi
	[[ $PAR_MIN_RUNNERS =~ ^[0-9]+$ && $PAR_MAX_RUNNERS =~ ^[1-9][0-9]*$ ]] &&
		((PAR_MIN_RUNNERS <= PAR_MAX_RUNNERS)) ||
		errors+=("PAR_MIN_RUNNERS and PAR_MAX_RUNNERS must be numbers with 0 <= min <= max and max >= 1")
	[[ $PAR_STORAGE =~ ^[A-Za-z][A-Za-z0-9._-]*$ ]] || errors+=("PAR_STORAGE is not a storage ID")
	[[ $PAR_BRIDGE =~ ^[A-Za-z][A-Za-z0-9._-]*$ ]] || errors+=("PAR_BRIDGE is not a bridge name")
	[[ -z $PAR_VLAN || ($PAR_VLAN =~ ^[0-9]+$ && $PAR_VLAN -ge 1 && $PAR_VLAN -le 4094) ]] ||
		errors+=("PAR_VLAN must be 1 to 4094")
	[[ -z $PAR_PVE_ADDRESS ]] || is_ipv4 "$PAR_PVE_ADDRESS" || errors+=("PAR_PVE_ADDRESS must be an IPv4 address")
	local name
	for name in PAR_GATEWAY_IP PAR_CONTROLLER_IP; do
		[[ ${!name} == dhcp ]] || is_cidr "${!name}" || errors+=("$name must be dhcp or an IPv4 CIDR")
	done
	if [[ $PAR_GATEWAY_IP != dhcp || $PAR_CONTROLLER_IP != dhcp ]]; then
		is_ipv4 "$PAR_LAN_GATEWAY" || errors+=("PAR_LAN_GATEWAY is required with a static address")
	fi
	is_cidr "$PAR_WORKER_SUBNET" || errors+=("PAR_WORKER_SUBNET must be an IPv4 CIDR")
	if [[ $PAR_VMID_START =~ ^[0-9]+$ && $PAR_VMID_END =~ ^[0-9]+$ ]]; then
		((PAR_VMID_START >= 100 && PAR_VMID_END <= 999999999)) || errors+=("VMIDs must be 100 to 999999999")
		((PAR_VMID_END - PAR_VMID_START + 1 >= PAR_MAX_RUNNERS + RESERVED_VMIDS)) ||
			errors+=("the VMID range needs room for PAR_MAX_RUNNERS workers plus $RESERVED_VMIDS reserved IDs")
	else
		errors+=("PAR_VMID_START and PAR_VMID_END must be numbers")
	fi
	if ((${#errors[@]} > 0)); then
		printf 'error: %s\n' "${errors[@]}" >&2
		exit 1
	fi
}

is_repository() { [[ ${PAR_GITHUB_URL#https://github.com/} == */* ]]; }

# ---------------------------------------------------------------------------------------------------------------
# IPv4 arithmetic

is_ipv4() {
	local IFS=. octet
	[[ $1 =~ ^[0-9]{1,3}(\.[0-9]{1,3}){3}$ ]] || return 1
	for octet in $1; do
		((10#$octet <= 255)) || return 1
	done
}

is_cidr() {
	[[ $1 == */* ]] && is_ipv4 "${1%/*}" && [[ ${1#*/} =~ ^[0-9]{1,2}$ ]] && ((${1#*/} <= 32))
}

ip_to_int() {
	local IFS=. a b c d
	read -r a b c d <<<"$1"
	echo $(((10#$a << 24) + (10#$b << 16) + (10#$c << 8) + 10#$d))
}

# cidrs_overlap reports whether two CIDRs (or plain addresses) share an address.
cidrs_overlap() {
	local a b pa=32 pb=32
	a=$(ip_to_int "${1%/*}")
	b=$(ip_to_int "${2%/*}")
	[[ $1 == */* ]] && pa=${1#*/}
	[[ $2 == */* ]] && pb=${2#*/}
	local p=$((pa < pb ? pa : pb))
	local mask=$(((0xffffffff << (32 - p)) & 0xffffffff))
	(((a & mask) == (b & mask)))
}

# network_of turns an address with a prefix into its network, such as 192.0.2.10/24 into 192.0.2.0/24.
network_of() {
	local n p=${1#*/}
	n=$(($(ip_to_int "${1%/*}") & ((0xffffffff << (32 - p)) & 0xffffffff)))
	echo "$((n >> 24 & 255)).$((n >> 16 & 255)).$((n >> 8 & 255)).$((n & 255))/$p"
}

# ---------------------------------------------------------------------------------------------------------------
# Proxmox queries

node_name() { pvesh get /nodes --output-format json | json 'print $d->[0]{node}'; }

# bridge_cidrs prints the host's IPv4 addresses with prefixes on the LAN bridge.
bridge_cidrs() { ip -4 -o addr show dev "$PAR_BRIDGE" 2>/dev/null | awk '{print $4}'; }

pve_address() {
	if [[ -n $PAR_PVE_ADDRESS ]]; then
		echo "$PAR_PVE_ADDRESS"
	else
		bridge_cidrs | head -n 1 | cut -d/ -f1
	fi
}

# served_cert prints the certificate file the API serves: a custom or ACME certificate if one is installed, otherwise
# the node's own, which the node's CA signs.
served_cert() {
	if [[ -e $PVE_DIR/local/pveproxy-ssl.pem ]]; then
		echo "$PVE_DIR/local/pveproxy-ssl.pem"
	else
		echo "$PVE_DIR/local/pve-ssl.pem"
	fi
}

# tls_mode prints how the controller verifies the API's certificate. Each way keeps working when the certificate is
# renewed, except pin:
#   ca      the node's own certificate, against the node's CA (pve-root-ca.pem), which renewals keep
#   system  a custom or ACME certificate that the system CAs trust
#   pin     a certificate from a CA the system doesn't trust: its fingerprint is pinned
tls_mode() {
	local cert
	cert=$(served_cert)
	if [[ $cert == */pve-ssl.pem ]]; then
		echo ca
	elif openssl verify -untrusted "$cert" "$cert" >/dev/null 2>&1; then
		echo system
	else
		echo pin
	fi
}

# tls_server_name prints a DNS name the served certificate is issued for, other than localhost. The controller
# connects by IP address and checks the certificate against this name.
tls_server_name() {
	openssl x509 -noout -ext subjectAltName -in "$(served_cert)" 2>/dev/null | tr ',' '\n' |
		sed -n 's/^[[:space:]]*DNS://p' | grep -vx localhost | head -n 1 || true
}

# tls_fingerprint prints the SHA-256 fingerprint of the served certificate, for tls_mode pin.
tls_fingerprint() { openssl x509 -noout -fingerprint -sha256 -in "$(served_cert)" | cut -d= -f2; }

# tls_config prints the proxmox: lines of the controller config that say how to verify the API.
tls_config() {
	local name
	case $(tls_mode) in
	ca) printf '  caCertFile: %s\n' "$CA_FILE" ;;
	pin)
		printf '  tlsFingerprint: "%s"\n' "$(tls_fingerprint)"
		return
		;;
	esac
	name=$(tls_server_name)
	[[ -z $name ]] || printf '  tlsServerName: %s\n' "$name"
}

# vms_with_tag prints the VMIDs in a pool that carry a tag, one per line.
vms_with_tag() {
	pvesh get /cluster/resources --type vm --output-format json | json '
		my ($pool, $tag) = @ARGV;
		for my $vm (sort { $a->{vmid} <=> $b->{vmid} } @$d) {
			next unless ($vm->{pool} // "") eq $pool;
			print "$vm->{vmid}\n" if grep { $_ eq $tag } split /[;,]/, $vm->{tags} // "";
		}' "$1" "$2"
}

# vm_tags prints a VM's tags, one per line.
vm_tags() { qm config "$1" | sed -n 's/^tags: //p' | tr ';,' '\n'; }

vm_has_tag() { vm_tags "$1" | grep -qx "$2"; }

# used_vmids prints every VMID on the node.
used_vmids() { pvesh get /cluster/resources --type vm --output-format json | json 'print "$_->{vmid}\n" for @$d'; }

# free_reserved_vmid prints the first unused VMID among the reserved IDs at the end of the range.
free_reserved_vmid() {
	local used id
	used=$(used_vmids)
	for ((id = PAR_VMID_END - RESERVED_VMIDS + 1; id <= PAR_VMID_END; id++)); do
		grep -qx "$id" <<<"$used" || {
			echo "$id"
			return 0
		}
	done
	return 1
}

exists_pool() { pveum pool list --output-format json | json 'exit !grep { $_->{poolid} eq $ARGV[0] } @$d' "$1"; }
exists_role() { pveum role list --output-format json | json 'exit !grep { $_->{roleid} eq $ARGV[0] } @$d' "$1"; }
exists_user() { pveum user list --output-format json | json 'exit !grep { $_->{userid} eq $ARGV[0] } @$d' "$1"; }
exists_token() {
	pveum user token list "$PVE_USER" --output-format json 2>/dev/null |
		json 'exit !grep { $_->{tokenid} eq $ARGV[0] } @$d' "$TOKEN"
}
exists_sdn() { pvesh get "/cluster/sdn/$1" >/dev/null 2>&1; }

# ---------------------------------------------------------------------------------------------------------------
# Guest agent

# wait_agent waits until a VM's guest agent answers.
wait_agent() {
	local vmid=$1 i
	for ((i = 0; i < 300; i += 2)); do
		qm guest cmd "$vmid" ping >/dev/null 2>&1 && return 0
		sleep 2
	done
	die "VM $vmid's guest agent didn't answer within 5 minutes"
}

# guest_exec runs a command in a VM through the guest agent: guest_exec VMID TIMEOUT [--stdin] -- COMMAND...
# It prints the command's stdout, passes on its stderr, and returns its exit status (124 if it didn't finish in
# TIMEOUT seconds). With --stdin, this function's stdin goes to the command, which is how secrets reach a VM.
guest_exec() {
	local vmid=$1 timeout=$2
	shift 2
	local args=(qm guest exec "$vmid" --timeout "$timeout")
	if [[ $1 == --stdin ]]; then
		args+=(--pass-stdin 1)
		shift
	fi
	[[ $1 == -- ]] && shift
	local out
	if [[ ${args[*]} == *--pass-stdin* ]]; then
		out=$("${args[@]}" -- "$@") || return 1
	else
		out=$("${args[@]}" -- "$@" </dev/null) || return 1
	fi
	json '
		print STDOUT $d->{"out-data"} // "";
		print STDERR $d->{"err-data"} // "";
		exit 124 unless $d->{exited};
		exit($d->{exitcode} // 1);' <<<"$out"
}

# as_parcon runs a command in the controller VM as the parcon user.
as_parcon() {
	local vmid=$1 timeout=$2
	shift 2
	local stdin=()
	if [[ $1 == --stdin ]]; then
		stdin=(--stdin)
		shift
	fi
	guest_exec "$vmid" "$timeout" "${stdin[@]}" -- runuser -u parcon -- "$@"
}

# write_controller_file writes stdin to a file in the controller VM, owned by parcon with mode 0600.
write_controller_file() {
	as_parcon "$1" 30 --stdin sh -c 'umask 077 && cat >"$1"' sh "$2"
}

# ---------------------------------------------------------------------------------------------------------------
# Preflight checks

# check runs one check: check hard|warn DESCRIPTION COMMAND... The command prints a reason when it fails.
check() {
	local level=$1 desc=$2 reason
	shift 2
	if reason=$("$@" 2>&1); then
		say "ok    $desc${reason:+: $reason}"
	elif [[ $level == hard ]]; then
		say "FAIL  $desc${reason:+: $reason}"
		PREFLIGHT_FAILED=1
	else
		say "WARN  $desc${reason:+: $reason}"
		WARNINGS+=("$desc${reason:+: $reason}")
	fi
}

check_root() { ((EUID == 0)) || { echo "run it as root" && return 1; }; }

check_pve9() {
	local v
	v=$(pveversion 2>/dev/null) || { echo "pveversion not found" && return 1; }
	echo "${v%% *}"
	[[ $v == pve-manager/9.* ]]
}

check_pve_node() {
	[[ -d /etc/pve/nodes ]] || { echo "/etc/pve isn't mounted" && return 1; }
	! systemd-detect-virt --container --quiet || { echo "running in a container" && return 1; }
}

check_amd64() {
	local arch
	arch=$(dpkg --print-architecture)
	[[ $arch == amd64 ]] || { echo "$arch" && return 1; }
}

check_kvm() { [[ -e /dev/kvm ]] || { echo "/dev/kvm is missing" && return 1; }; }

check_tools() {
	local tool missing=()
	for tool in qm pveum pvesh pvesm curl sha256sum perl openssl; do
		command -v "$tool" >/dev/null || missing+=("$tool")
	done
	((${#missing[@]} == 0)) || { echo "missing ${missing[*]}" && return 1; }
}

check_standalone() {
	[[ ! -e /etc/pve/corosync.conf ]] || { echo "this node is in a cluster; clusters aren't supported" && return 1; }
}

check_clock() {
	[[ $(timedatectl show -p NTPSynchronized --value 2>/dev/null) == yes ]] ||
		{ echo "the clock isn't synchronized; GitHub App tokens fail when it drifts" && return 1; }
}

check_https() {
	local host failed=()
	for host in github.com api.github.com objects.githubusercontent.com release-assets.githubusercontent.com; do
		curl -sS -o /dev/null --max-time 10 "https://$host/" 2>/dev/null || failed+=("$host")
	done
	((${#failed[@]} == 0)) || { echo "can't reach ${failed[*]}" && return 1; }
}

# storage_json prints the target storage's status.
storage_json() { pvesh get "/nodes/$(node_name)/storage/$PAR_STORAGE/status" --output-format json 2>/dev/null; }

check_storage() {
	local status
	status=$(storage_json) || { echo "no storage $PAR_STORAGE on this node" && return 1; }
	json 'exit !($d->{active} && $d->{enabled})' <<<"$status" || { echo "$PAR_STORAGE isn't active" && return 1; }
	json 'exit !grep { $_ eq "images" } split /,/, $d->{content} // ""' <<<"$status" ||
		{ echo "$PAR_STORAGE doesn't accept VM disks (content images)" && return 1; }
	json 'print $d->{type}' <<<"$status"
}

linked_clones_supported() {
	local type
	type=$(storage_json | json 'print $d->{type} // ""')
	case $type in
	lvmthin | zfspool | rbd | dir | nfs | cifs | glusterfs | btrfs) return 0 ;;
	*)
		echo "$type storage can't make linked clones; workers will be full clones (slower, more space)"
		return 1
		;;
	esac
}

check_linked_clones() { linked_clones_supported; }

check_free_space() {
	local avail need per_worker=$((14))
	avail=$(storage_json | json 'print int(($d->{avail} // 0) / 2**30)')
	linked_clones_supported >/dev/null || per_worker=$((TEMPLATE_GIB + 14))
	need=$((GATEWAY_GIB + CONTROLLER_GIB + TEMPLATE_GIB + PAR_MAX_RUNNERS * per_worker + DOWNLOAD_GIB))
	echo "$avail GiB free, about $need GiB needed"
	((avail >= need))
}

check_memory() {
	local total used need
	total=$(($(awk '/^MemTotal:/ {print $2}' /proc/meminfo) / 1024))
	used=$(pvesh get /cluster/resources --type vm --output-format json |
		json 'my $m = 0; $m += ($_->{maxmem} // 0) for grep { !$_->{template} } @$d; print int($m / 2**20)')
	need=$((used + PAR_MAX_RUNNERS * WORKER_MEMORY_MIB + 3072))
	echo "$total MiB RAM, $need MiB allocated to VMs with $PAR_MAX_RUNNERS workers"
	((total >= need))
}

check_bridge() {
	[[ -d /sys/class/net/$PAR_BRIDGE/bridge ]] || { echo "no bridge $PAR_BRIDGE" && return 1; }
	[[ -n $(pve_address) ]] || { echo "set PAR_PVE_ADDRESS: $PAR_BRIDGE has no IPv4 address" && return 1; }
	pve_address
}

check_sdn() {
	dpkg-query -W -f '${Status}' ifupdown2 2>/dev/null | grep -q 'install ok installed' ||
		{ echo "ifupdown2 isn't installed" && return 1; }
	grep -Eq '^[[:space:]]*source[[:space:]]+/etc/network/interfaces\.d/\*' /etc/network/interfaces ||
		{ echo "/etc/network/interfaces doesn't source /etc/network/interfaces.d/*" && return 1; }
}

check_worker_subnet() {
	local cidr
	for cidr in $(ip -4 -o addr show | awk '{print $4}') $(ip -4 route show | awk '$1 != "default" {print $1}'); do
		[[ $cidr == */* ]] || cidr=$cidr/32
		is_cidr "$cidr" || continue
		if cidrs_overlap "$PAR_WORKER_SUBNET" "$cidr"; then
			[[ $cidr == "$PAR_WORKER_SUBNET" ]] && ip -4 route show "$cidr" | grep -q "dev $VNET" && continue
			echo "$PAR_WORKER_SUBNET overlaps $cidr on this host; set PAR_WORKER_SUBNET"
			return 1
		fi
	done
	for cidr in $PAR_GATEWAY_IP $PAR_CONTROLLER_IP; do
		[[ $cidr == dhcp ]] && continue
		! cidrs_overlap "$PAR_WORKER_SUBNET" "$cidr" || { echo "$PAR_WORKER_SUBNET overlaps the LAN $cidr" && return 1; }
	done
}

# ours reports whether an earlier run created our objects: it creates the system pool first, with this comment.
readonly POOL_COMMENT=proxmox-actions-runners
ours() {
	pveum pool list --output-format json |
		json 'exit !grep { $_->{poolid} eq $ARGV[0] && ($_->{comment} // "") eq $ARGV[1] } @$d' \
			"$SYSTEM_POOL" "$POOL_COMMENT"
}

installed_controller() {
	exists_pool "$SYSTEM_POOL" || return 0
	vms_with_tag "$SYSTEM_POOL" "$TAG_CONTROLLER" | head -n 1
}

# check_tls reports how the controller will verify the API's certificate. Only a pinned certificate needs attention.
check_tls() {
	case $(tls_mode) in
	ca) echo "the node's own certificate, verified against the node's CA" ;;
	system) echo "a certificate the system CAs trust, verified as $(tls_server_name)" ;;
	pin)
		echo "a certificate from a CA the host doesn't trust: the controller pins it, and after it is renewed the" \
			"controller's config needs its new fingerprint"
		return 1
		;;
	esac
}

check_names() {
	ours && { echo "found an earlier install; continuing it" && return 0; }
	local taken=()
	exists_sdn "zones/$ZONE" && taken+=("SDN zone $ZONE")
	exists_sdn "vnets/$VNET" && taken+=("SDN VNet $VNET")
	exists_pool "$RUNNER_POOL" && taken+=("pool $RUNNER_POOL")
	exists_role "$ROLE" && taken+=("role $ROLE")
	exists_user "$PVE_USER" && taken+=("user $PVE_USER")
	((${#taken[@]} == 0)) && return 0
	echo "already exist and weren't created by this installer: ${taken[*]}"
	return 1
}

check_vmids() {
	local used clash=() id pool
	used=$(pvesh get /cluster/resources --type vm --output-format json |
		json 'print "$_->{vmid} ", ($_->{pool} // ""), "\n" for @$d')
	while read -r id pool; do
		((id >= PAR_VMID_START && id <= PAR_VMID_END)) || continue
		[[ $pool == "$RUNNER_POOL" ]] || clash+=("$id")
	done <<<"$used"
	((${#clash[@]} == 0)) ||
		{ echo "VMIDs ${clash[*]} in $PAR_VMID_START-$PAR_VMID_END belong to other VMs; choose another range" && return 1; }
}

check_pending_sdn() {
	local pending
	pending=$(pvesh get /cluster/sdn/zones --pending 1 --output-format json 2>/dev/null |
		json 'print join(" ", map { $_->{zone} } grep { $_->{state} } @$d)') || true
	[[ -z $pending ]] || { echo "pending SDN changes to $pending would be applied too" && return 1; }
}

preflight() {
	PREFLIGHT_FAILED=0
	step "Preflight checks"
	check hard "running as root" check_root
	check hard "Proxmox VE 9.x" check_pve9
	check hard "running on a Proxmox VE node" check_pve_node
	check hard "amd64" check_amd64
	check hard "KVM available" check_kvm
	check hard "required tools present" check_tools
	check hard "standalone node" check_standalone
	check hard "clock synchronized" check_clock
	check hard "outbound HTTPS to GitHub" check_https
	check hard "storage $PAR_STORAGE" check_storage
	check warn "linked clones on $PAR_STORAGE" check_linked_clones
	check hard "free space on $PAR_STORAGE" check_free_space
	check warn "memory" check_memory
	check hard "LAN bridge $PAR_BRIDGE" check_bridge
	check warn "API certificate" check_tls
	check hard "SDN available" check_sdn
	check hard "worker subnet $PAR_WORKER_SUBNET free" check_worker_subnet
	check hard "names free" check_names
	check hard "VMIDs $PAR_VMID_START-$PAR_VMID_END free" check_vmids
	check warn "no pending SDN changes" check_pending_sdn
	((PREFLIGHT_FAILED == 0)) || die "preflight checks failed"
}

# ---------------------------------------------------------------------------------------------------------------
# Release assets

require_release() {
	[[ $PAR_VERSION != @*@ ]] ||
		die "this install.sh is from the source tree; download install.sh from a release: $REPO_URL/releases"
}

asset_url() { echo "$REPO_URL/releases/download/v$PAR_VERSION/$1"; }

asset_sha256() {
	awk -v name="$1" '$2 == name || $2 == "*" name {print $1}' <<<"$PAR_SHA256SUMS"
}

# download fetches a release asset into WORK_DIR and checks it against the checksum embedded in this script.
download() {
	local name=$1 sum
	sum=$(asset_sha256 "$name")
	[[ -n $sum ]] || die "no checksum for $name in this install.sh"
	if [[ ! -f $WORK_DIR/$name ]]; then
		say "    downloading $name"
		curl -fsSL --retry 3 -o "$WORK_DIR/$name.part" "$(asset_url "$name")" || die "downloading $name failed"
		mv "$WORK_DIR/$name.part" "$WORK_DIR/$name"
	fi
	echo "$sum  $WORK_DIR/$name" | sha256sum -c --quiet - || die "$name doesn't match its checksum"
}

make_work_dir() {
	WORK_DIR=$(mktemp -d /var/tmp/par-install.XXXXXX)
	trap 'rm -rf "$WORK_DIR"' EXIT
}

runner_version() {
	json 'print $d->{builds}[-1]{custom_data}{runner_version}' <"$WORK_DIR/par-runner-$PAR_VERSION.json"
}

# ---------------------------------------------------------------------------------------------------------------
# Install steps

show_plan() {
	step "Plan"
	cat <<EOF
Proxmox objects on node $(node_name):
  pools $SYSTEM_POOL and $RUNNER_POOL
  role $ROLE, user $PVE_USER, token $PVE_USER!$TOKEN, and ACLs for them on /pool/$RUNNER_POOL,
    /storage/$PAR_STORAGE, and /sdn/zones/$ZONE/$VNET
  SDN zone $ZONE and VNet $VNET (no subnet, no host address), then apply the SDN config
  runner template in $RUNNER_POOL, VMIDs $((PAR_VMID_END - RESERVED_VMIDS + 1))-$PAR_VMID_END reserved for templates
  gateway VM in $SYSTEM_POOL: 1 vCPU, 1 GiB, ${GATEWAY_GIB} GiB disk, LAN $PAR_BRIDGE${PAR_VLAN:+ VLAN $PAR_VLAN} ($PAR_GATEWAY_IP), $VNET
  controller VM in $SYSTEM_POOL: 2 vCPU, 2 GiB, ${CONTROLLER_GIB} GiB disk, LAN $PAR_BRIDGE${PAR_VLAN:+ VLAN $PAR_VLAN} ($PAR_CONTROLLER_IP)
Workers: VMIDs $PAR_VMID_START-$((PAR_VMID_END - RESERVED_VMIDS)) on $VNET ($PAR_WORKER_SUBNET), at most $PAR_MAX_RUNNERS
GitHub: scale set $PAR_SCALE_SET (runs-on: $PAR_LABELS) for $PAR_GITHUB_URL, $PAR_GITHUB_APP App
EOF
	if ((${#WARNINGS[@]} > 0)); then
		say "Warnings:"
		printf '  %s\n' "${WARNINGS[@]}"
	fi
}

create_access() {
	step "Proxmox access objects"
	local pool
	for pool in "$SYSTEM_POOL" "$RUNNER_POOL"; do
		exists_pool "$pool" || change pveum pool add "$pool" --comment "$POOL_COMMENT"
	done
	local privs="${PRIVILEGES[*]}"
	if exists_role "$ROLE"; then
		change pveum role modify "$ROLE" --privs "${privs// /,}"
	else
		change pveum role add "$ROLE" --privs "${privs// /,}"
	fi
	exists_user "$PVE_USER" || change pveum user add "$PVE_USER" --comment "proxmox-actions-runners controller"
}

# grant_acls gives the user and its token the role on each path: a privilege-separated token gets only what both
# have.
grant_acls() {
	local path
	for path in "/pool/$RUNNER_POOL" "/storage/$PAR_STORAGE" "/sdn/zones/$ZONE/$VNET"; do
		change pveum acl modify "$path" --users "$PVE_USER" --tokens "$PVE_USER!$TOKEN" --roles "$ROLE"
	done
}

create_network() {
	step "Worker network"
	local changed=0
	exists_sdn "zones/$ZONE" || { change pvesh create /cluster/sdn/zones --type simple --zone "$ZONE" && changed=1; }
	exists_sdn "vnets/$VNET" || { change pvesh create /cluster/sdn/vnets --vnet "$VNET" --zone "$ZONE" && changed=1; }
	if ((changed)); then
		change pvesh set /cluster/sdn
	fi
	local i
	for ((i = 0; i < 60; i++)); do
		[[ -e /sys/class/net/$VNET ]] && return 0
		sleep 1
	done
	die "VNet $VNET didn't come up after applying the SDN config"
}

download_images() {
	step "Download and verify images"
	make_work_dir
	download "par-runner-$PAR_VERSION.qcow2"
	download "par-runner-$PAR_VERSION.json"
	download "par-gateway-$PAR_VERSION.qcow2"
	download "parcon-$PAR_VERSION.qcow2"
}

net_config() { # net_config BRIDGE: a virtio NIC on the LAN bridge, with the VLAN tag if set
	echo "virtio,bridge=$PAR_BRIDGE${PAR_VLAN:+,tag=$PAR_VLAN}"
}

ip_config() { # ip_config ADDRESS: cloud-init's ipconfig for dhcp or a static CIDR
	if [[ $1 == dhcp ]]; then
		echo "ip=dhcp"
	else
		echo "ip=$1,gw=$PAR_LAN_GATEWAY"
	fi
}

release_tag() { echo "$RELEASE_TAG_PREFIX$PAR_VERSION"; }

# import_template imports the runner image as a new template in the reserved VMIDs, unless one from this release
# exists.
import_template() {
	step "Runner template"
	local vmid
	vmid=$(release_template)
	if [[ -n $vmid ]]; then
		if qm config "$vmid" | grep -qx 'template: 1'; then
			say "    template $vmid is from this release"
			return 0
		fi
		say "    VM $vmid is a template import a failed run left; importing again"
		change qm destroy "$vmid" --purge 1
	fi
	vmid=$(free_reserved_vmid) || die "no free VMID in the reserved IDs at the end of $PAR_VMID_START-$PAR_VMID_END"
	# The template tags come first, so a failed import is found and replaced by the next run. The controller only
	# clones a VM that Proxmox reports as a template.
	local tags
	tags="$TAG_MANAGED;$TAG_TEMPLATE;par-tv-$(date +%s);par-rv-$(runner_version);$(release_tag)"
	change qm create "$vmid" --name "par-runner-${PAR_VERSION//./-}" --pool "$RUNNER_POOL" --memory 2048 --cores 2 \
		--cpu host --ostype l26 --scsihw virtio-scsi-single --net0 "virtio,bridge=$VNET" --agent enabled=1 \
		--serial0 socket --vga serial0 --tags "$tags"
	change qm set "$vmid" --scsi0 "$PAR_STORAGE:0,import-from=$WORK_DIR/par-runner-$PAR_VERSION.qcow2"
	change qm set "$vmid" --ide2 "$PAR_STORAGE:cloudinit" --boot order=scsi0 --ipconfig0 ip=dhcp
	change qm template "$vmid"
}

# release_template prints the VMID of this release's runner template, if it was imported.
release_template() { vms_with_tag "$RUNNER_POOL" "$(release_tag)" | head -n 1; }

# create_system_vm creates the gateway or controller VM from its image: create_system_vm VMID NAME IMAGE CORES
# MEMORY_MIB DISK_GIB TAG ADDRESS [NET1]
create_system_vm() {
	local vmid=$1 name=$2 image=$3 cores=$4 memory=$5 disk=$6 tag=$7 address=$8 net1=${9:-}
	local args=(--name "$name" --pool "$SYSTEM_POOL" --memory "$memory" --cores "$cores" --cpu host --ostype l26
		--scsihw virtio-scsi-single --net0 "$(net_config)" --agent enabled=1 --onboot 1 --serial0 socket
		--vga serial0 --tags "$TAG_MANAGED;$tag")
	[[ -z $net1 ]] || args+=(--net1 "$net1")
	change qm create "$vmid" "${args[@]}"
	change qm set "$vmid" --scsi0 "$PAR_STORAGE:0,import-from=$WORK_DIR/$image"
	change qm set "$vmid" --ide2 "$PAR_STORAGE:cloudinit" --boot order=scsi0 --ipconfig0 "$(ip_config "$address")" \
		--ciupgrade 0
	if ((disk > GATEWAY_GIB)); then
		change qm disk resize "$vmid" scsi0 "${disk}G"
	fi
	change qm start "$vmid"
}

next_vmid() { pvesh get /cluster/nextid; }

# system_vm prints the VMID of the gateway or controller VM, if it exists. One that a failed run left without its
# disk is destroyed, and one that is stopped is started.
system_vm() {
	local vmid
	vmid=$(vms_with_tag "$SYSTEM_POOL" "$1" | head -n 1)
	[[ -n $vmid ]] || return 0
	if ! qm config "$vmid" | grep -q '^scsi0:'; then
		change qm destroy "$vmid" --purge 1 >&2
		return 0
	fi
	qm status "$vmid" | grep -q running || change qm start "$vmid" >&2
	echo "$vmid"
}

# vm_ipv4 prints a VM's first global IPv4 address on its first NIC, from the guest agent.
vm_ipv4() {
	local i address
	for ((i = 0; i < 60; i++)); do
		address=$(qm guest cmd "$1" network-get-interfaces 2>/dev/null | json '
			for my $if (@$d) {
				next if $if->{name} eq "lo" || $if->{name} eq "net1";
				for my $a (@{$if->{"ip-addresses"} // []}) {
					next unless $a->{"ip-address-type"} eq "ipv4" && $a->{"ip-address"} !~ /^169\.254\./;
					print $a->{"ip-address"}; exit;
				}
			}') || true
		[[ -n $address ]] && {
			echo "$address"
			return 0
		}
		sleep 2
	done
	return 1
}

create_gateway() {
	step "Gateway VM"
	local vmid
	vmid=$(system_vm "$TAG_GATEWAY")
	if [[ -z $vmid ]]; then
		vmid=$(next_vmid)
		create_system_vm "$vmid" par-gateway "par-gateway-$PAR_VERSION.qcow2" 1 1024 "$GATEWAY_GIB" \
			"$TAG_GATEWAY;$(release_tag)" "$PAR_GATEWAY_IP" "virtio,bridge=$VNET"
	fi
	GATEWAY_VMID=$vmid
	wait_agent "$vmid"
	configure_gateway "$vmid"
}

# gateway_block prints what workers must not reach: the host's networks on the LAN bridge, the API address, and the
# controller VM once it has one. RFC 1918, CGNAT, and link-local ranges are always blocked by the gateway itself.
gateway_block() {
	local cidr block=()
	for cidr in $(bridge_cidrs); do
		block+=("$(network_of "$cidr")")
	done
	block+=("$(pve_address)")
	[[ -z ${CONTROLLER_ADDRESS:-} ]] || block+=("$CONTROLLER_ADDRESS")
	[[ $PAR_CONTROLLER_IP == dhcp ]] || block+=("${PAR_CONTROLLER_IP%/*}")
	printf '%s\n' "${block[@]}" | sort -u | tr '\n' ' ' | sed 's/ $//'
}

configure_gateway() {
	local vmid=$1
	say "    configuring gateway $vmid: worker subnet $PAR_WORKER_SUBNET, blocking $(gateway_block)"
	((DRY_RUN)) && return 0
	printf 'WORKER_SUBNET=%s\nBLOCK=%s\n' "$PAR_WORKER_SUBNET" "$(gateway_block)" |
		guest_exec "$vmid" 60 --stdin -- /usr/local/sbin/par-gateway-configure >/dev/null ||
		die "configuring the gateway failed"
	guest_exec "$vmid" 30 -- curl -sS -o /dev/null --max-time 10 https://api.github.com/ ||
		die "the gateway VM can't reach GitHub"
}

create_controller() {
	step "Controller VM"
	local vmid
	vmid=$(system_vm "$TAG_CONTROLLER")
	if [[ -z $vmid ]]; then
		vmid=$(next_vmid)
		create_system_vm "$vmid" par-controller "parcon-$PAR_VERSION.qcow2" 2 2048 "$CONTROLLER_GIB" \
			"$TAG_CONTROLLER" "$PAR_CONTROLLER_IP"
	fi
	CONTROLLER_VMID=$vmid
	wait_agent "$vmid"
	CONTROLLER_ADDRESS=$(vm_ipv4 "$vmid") || die "the controller VM got no IPv4 address"
	say "    controller $vmid at $CONTROLLER_ADDRESS"
	# The gateway was configured before the controller had an address; block it now.
	configure_gateway "$GATEWAY_VMID"
}

# render_config prints the controller's config.yaml. With a Client ID, it names the GitHub App; the installation ID
# follows once the App is installed.
render_config() {
	local client_id=${1:-} installation_id=${2:-} labels=${PAR_LABELS//,/, }
	cat <<EOF
# Written by install.sh. Rewritten on install; upgrade keeps it.
proxmox:
  url: https://$(pve_address):8006/api2/json
  tokenId: $PVE_USER!$TOKEN
  tokenSecretFile: $ETC/pve-token
$(tls_config)
  node: $(node_name)
  pool: $RUNNER_POOL
  storage: $PAR_STORAGE
  zone: $ZONE
  vnet: $VNET
  vmidRange: { start: $PAR_VMID_START, end: $PAR_VMID_END }
  linkedClone: $(linked_clones_supported >/dev/null && echo true || echo false)

github:
  configUrl: $PAR_GITHUB_URL
EOF
	if [[ -n $client_id ]]; then
		printf '  app:\n    clientId: %s\n' "$client_id"
		[[ -z $installation_id ]] || printf '    installationId: %s\n' "$installation_id"
		printf '    privateKeyFile: %s\n' "$KEY_FILE"
	fi
	cat <<EOF

scaleSets:
  - name: $PAR_SCALE_SET
    labels: [$labels]
    runnerGroup: "$PAR_RUNNER_GROUP"
    minRunners: $PAR_MIN_RUNNERS
    maxRunners: $PAR_MAX_RUNNERS
EOF
}

# configure_controller writes the config without the App and the API token, then checks Proxmox from the VM.
configure_controller() {
	step "Configure the controller"
	((DRY_RUN)) && return 0
	local vmid=$CONTROLLER_VMID client_id installation_id
	read -r client_id installation_id <<<"$(read_existing_app)" || true
	if [[ $(tls_mode) == ca ]]; then
		write_controller_file "$vmid" "$CA_FILE" <"$PVE_DIR/pve-root-ca.pem" || die "writing the node's CA failed"
	fi
	render_config "$client_id" "$installation_id" | write_controller_file "$vmid" "$ETC/config.yaml" ||
		die "writing the config failed"

	# A token's secret is shown only when it's created. Keep a token the controller already holds; otherwise make a
	# new one and pipe its secret straight into the VM.
	if exists_token && as_parcon "$vmid" 30 test -s "$ETC/pve-token"; then
		say "    the controller already holds token $PVE_USER!$TOKEN"
	else
		if exists_token; then
			change pveum user token remove "$PVE_USER" "$TOKEN"
		fi
		local secret
		secret=$(pveum user token add "$PVE_USER" "$TOKEN" --privsep 1 --comment "proxmox-actions-runners controller" \
			--output-format json | json 'print $d->{value} // ""')
		[[ -n $secret ]] || die "creating the API token returned no secret"
		printf '%s\n' "$secret" | write_controller_file "$vmid" "$ETC/pve-token" || die "writing the token failed"
		secret=""
		say "    created token $PVE_USER!$TOKEN and wrote its secret to the controller"
	fi
	grant_acls
	as_parcon "$vmid" 60 parcon check proxmox || die "parcon check proxmox failed in the controller VM"
}

# app_query prints the helper page's query string for the target, without the state.
app_query() {
	local path=${PAR_GITHUB_URL#https://github.com/} owner repo type
	owner=${path%%/*}
	if [[ $path != */* ]]; then
		echo "org=$owner"
		return
	fi
	repo=${path#*/}
	type=$(curl -fsSL --max-time 15 -H 'Accept: application/vnd.github+json' "https://api.github.com/users/$owner" |
		json 'print $d->{type} // ""') || die "can't look up $owner on GitHub"
	if [[ $type == Organization ]]; then
		echo "org=$owner&repo=$repo"
	else
		echo "user=$owner&repo=$repo"
	fi
}

# read_existing_app prints the Client ID and installation ID already in the controller's config, if any.
read_existing_app() {
	as_parcon "$CONTROLLER_VMID" 30 cat "$ETC/config.yaml" 2>/dev/null |
		sed -n 's/^ *\(clientId\|installationId\): *//p' | tr '\n' ' '
}

# setup_app creates or imports the GitHub App and waits for its installation. The Client ID goes into the config as
# soon as the key is in the controller, so a re-run after a failure continues with the same App instead of creating
# another.
setup_app() {
	step "GitHub App"
	((DRY_RUN)) && return 0
	local vmid=$CONTROLLER_VMID client_id="" installation_id=""
	read -r client_id installation_id <<<"$(read_existing_app)" || true
	if [[ -z $client_id ]]; then
		if [[ $PAR_GITHUB_APP == manual ]]; then
			client_id=$PAR_GITHUB_APP_CLIENT_ID
			import_app_key "$vmid"
		else
			client_id=$(create_app "$vmid") || die "creating the GitHub App failed"
		fi
		render_config "$client_id" | write_controller_file "$vmid" "$ETC/config.yaml" || die "writing the config failed"
	fi
	if [[ -z $installation_id ]]; then
		say "    waiting for App $client_id to be installed on $PAR_GITHUB_URL (up to 15 minutes)"
		installation_id=$(as_parcon "$vmid" 960 parcon github app wait-installation -client-id "$client_id" \
			-target "$PAR_GITHUB_URL") || die "the App wasn't installed on $PAR_GITHUB_URL; run install again to keep waiting"
		render_config "$client_id" "$installation_id" | write_controller_file "$vmid" "$ETC/config.yaml" ||
			die "writing the config failed"
	fi
	as_parcon "$vmid" 60 parcon check github ||
		die "parcon check github failed in the controller VM with App $client_id; if the App was deleted in GitHub," \
			"uninstall and install again"
}

# create_app runs the manifest flow and prints the new App's Client ID. The code the user pastes goes only into the
# controller VM, on stdin.
create_app() {
	local vmid=$1 state line code out slug client_id
	state=$(head -c 18 /dev/urandom | base64 | tr '+/' '-_')
	cat >&2 <<EOF

    Create the GitHub App in a browser on any device:

      $HELPER_URL?$(app_query)&state=$state

    Review the App on GitHub and create it. The page then shows one line to copy. Paste it here.

EOF
	read -r -p "    Line from the page: " line </dev/tty
	[[ ${line%%.*} == "$state" && $line == *.* ]] || die "that line isn't from this install's link; run it again"
	code=${line#*.}
	line=""
	out=$(printf '%s\n' "$code" | as_parcon "$vmid" 60 --stdin parcon github app create) || return 1
	code=""
	client_id=$(json 'print $d->{clientId}' <<<"$out")
	slug=$(json 'print $d->{slug}' <<<"$out")
	cat >&2 <<EOF

    Created GitHub App $slug. Install it on $PAR_GITHUB_URL:

      https://github.com/apps/$slug/installations/new

EOF
	echo "$client_id"
}

# import_app_key reads an existing App's private key from the terminal and pipes it into the controller VM.
import_app_key() {
	local vmid=$1 key="" line
	[[ -r /dev/tty ]] || die "PAR_GITHUB_APP=manual reads the App's private key from the terminal"
	say "    Paste the App's private key (the whole PEM, ending with its END line):"
	while IFS= read -r line </dev/tty; do
		key+=$line$'\n'
		[[ $line == "-----END "* ]] && break
	done
	printf '%s' "$key" | as_parcon "$vmid" 30 --stdin parcon github app import || die "importing the key failed"
	key=""
}

start_controller() {
	step "Start the controller"
	((DRY_RUN)) && return 0
	SERVICE_STARTED=$(date +%s)
	guest_exec "$CONTROLLER_VMID" 60 -- systemctl enable --now parcon.service >/dev/null ||
		die "starting parcon.service failed"
}

# smoke_test clones a worker into a reserved VMID and checks its network from inside, then waits for the controller's
# scale set session.
smoke_test() {
	step "Smoke test"
	((DRY_RUN)) && return 0
	local vmid template
	for vmid in $(vms_with_tag "$RUNNER_POOL" "$TAG_BUILD"); do
		change qm stop "$vmid" --skiplock 1 >/dev/null 2>&1 || true
		change qm destroy "$vmid" --purge 1
	done
	template=$(release_template)
	vmid=$(free_reserved_vmid) || die "no free reserved VMID for the smoke test"
	change qm clone "$template" "$vmid" --name par-smoke-test --pool "$RUNNER_POOL"
	change qm set "$vmid" --tags "$TAG_MANAGED;$TAG_BUILD" --ciupgrade 0
	change qm start "$vmid"
	wait_agent "$vmid"
	local failures=()
	vm_ipv4 "$vmid" >/dev/null || failures+=("the worker got no DHCP lease")
	guest_exec "$vmid" 30 -- curl -sS -o /dev/null --max-time 15 https://api.github.com/ ||
		failures+=("the worker can't reach GitHub")
	! guest_exec "$vmid" 30 -- curl -sk -o /dev/null --max-time 5 "https://$(pve_address):8006/" 2>/dev/null ||
		failures+=("the worker can reach the Proxmox API")
	! guest_exec "$vmid" 30 -- timeout 5 bash -c "</dev/tcp/$CONTROLLER_ADDRESS/22" 2>/dev/null ||
		failures+=("the worker can reach the controller VM")
	change qm stop "$vmid" --skiplock 1
	change qm destroy "$vmid" --purge 1
	((${#failures[@]} == 0)) || die "smoke test: ${failures[*]}"
	say "    the worker got a lease, reaches GitHub, and can't reach the Proxmox API or the controller"

	local i
	for ((i = 0; i < 60; i++)); do
		if guest_exec "$CONTROLLER_VMID" 30 -- journalctl -u parcon.service --since "@$SERVICE_STARTED" -o cat |
			grep -q 'scale set session opened'; then
			say "    the controller registered scale set $PAR_SCALE_SET and is listening for jobs"
			return 0
		fi
		sleep 5
	done
	die "the controller didn't open its scale set session within 5 minutes; see journalctl -u parcon in VM $CONTROLLER_VMID"
}

finish_install() {
	change qm set "$CONTROLLER_VMID" --tags "$TAG_MANAGED;$TAG_CONTROLLER;$(release_tag)"
	step "Done"
	cat <<EOF
Use the runners in a workflow:

  runs-on: $PAR_LABELS

Controller VM $CONTROLLER_VMID ($CONTROLLER_ADDRESS), gateway VM $GATEWAY_VMID.
Upgrade:   bash install.sh upgrade
Uninstall: bash install.sh uninstall
EOF
}

do_install() {
	if [[ -n $(installed_release) ]]; then
		do_upgrade
		return
	fi
	[[ -z $ANSWERS ]] || load_answers "$ANSWERS"
	apply_defaults
	prompt_settings
	validate_settings
	preflight
	show_plan
	((DRY_RUN)) && return 0
	require_release
	confirm "Install proxmox-actions-runners $PAR_VERSION with this plan?" || die "cancelled"
	create_access
	create_network
	download_images
	import_template
	create_gateway
	create_controller
	configure_controller
	setup_app
	start_controller
	smoke_test
	finish_install
}

# installed_release prints the release of a finished install: the controller VM's release tag.
installed_release() {
	local vmid
	vmid=$(installed_controller)
	[[ -n $vmid ]] || return 0
	vm_tags "$vmid" | sed -n "s/^$RELEASE_TAG_PREFIX//p"
}

# ---------------------------------------------------------------------------------------------------------------
# Upgrade

# load_installed_settings reads the settings an upgrade needs from the running install instead of asking again.
load_installed_settings() {
	local config
	config=$(as_parcon "$CONTROLLER_VMID" 30 cat "$ETC/config.yaml") || die "can't read the controller's config"
	PAR_STORAGE=$(sed -n 's/^  storage: *//p' <<<"$config")
	PAR_VMID_START=$(sed -n 's/.*vmidRange: *{ *start: *\([0-9]*\).*/\1/p' <<<"$config")
	PAR_VMID_END=$(sed -n 's/.*vmidRange:.*end: *\([0-9]*\).*/\1/p' <<<"$config")
	[[ -n $PAR_STORAGE && -n $PAR_VMID_START && -n $PAR_VMID_END ]] || die "the controller's config is incomplete"
}

# replace_gateway recreates the gateway VM from the new image with the old one's VMID, NICs, address, and settings.
# Workers lose their network for the minute or two this takes.
replace_gateway() {
	step "Replace the gateway VM"
	local vmid=$GATEWAY_VMID config settings net0 net1 ipconfig0
	config=$(qm config "$vmid")
	net0=$(sed -n 's/^net0: //p' <<<"$config")
	net1=$(sed -n 's/^net1: //p' <<<"$config")
	ipconfig0=$(sed -n 's/^ipconfig0: //p' <<<"$config")
	settings=$(guest_exec "$vmid" 30 -- cat /etc/par-gateway/config) || die "can't read the gateway's settings"
	change qm stop "$vmid"
	change qm destroy "$vmid" --purge 1
	change qm create "$vmid" --name par-gateway --pool "$SYSTEM_POOL" --memory 1024 --cores 1 --cpu host --ostype l26 \
		--scsihw virtio-scsi-single --net0 "$net0" --net1 "$net1" --agent enabled=1 --onboot 1 --serial0 socket \
		--vga serial0 --tags "$TAG_MANAGED;$TAG_GATEWAY;$(release_tag)"
	change qm set "$vmid" --scsi0 "$PAR_STORAGE:0,import-from=$WORK_DIR/par-gateway-$PAR_VERSION.qcow2"
	change qm set "$vmid" --ide2 "$PAR_STORAGE:cloudinit" --boot order=scsi0 --ipconfig0 "$ipconfig0" --ciupgrade 0
	change qm start "$vmid"
	wait_agent "$vmid"
	guest_exec "$vmid" 60 --stdin -- /usr/local/sbin/par-gateway-configure <<<"$settings" >/dev/null ||
		die "configuring the new gateway failed"
}

# upgrade_controller has the controller VM download the new parcon binary, check it against the checksum embedded in
# this script, and restart the service. Config and secrets stay where they are.
upgrade_controller() {
	step "Upgrade the controller"
	local name="parcon-$PAR_VERSION-linux-amd64" sum
	sum=$(asset_sha256 "$name")
	[[ -n $sum ]] || die "no checksum for $name in this install.sh"
	say "    the controller VM downloads $name"
	guest_exec "$CONTROLLER_VMID" 300 -- sh -c 'set -e
		curl -fsSL --retry 3 -o /usr/local/bin/parcon.new "$1"
		echo "$2  /usr/local/bin/parcon.new" | sha256sum -c --quiet -
		chmod 0755 /usr/local/bin/parcon.new
		mv /usr/local/bin/parcon.new /usr/local/bin/parcon
		systemctl restart parcon.service' sh "$(asset_url "$name")" "$sum" || die "upgrading the controller failed"
	change qm set "$CONTROLLER_VMID" --tags "$TAG_MANAGED;$TAG_CONTROLLER;$(release_tag)"
}

do_upgrade() {
	check_root >/dev/null || die "run it as root"
	local release
	release=$(installed_release)
	[[ -n $release ]] || die "no finished install found; run install"
	CONTROLLER_VMID=$(installed_controller)
	GATEWAY_VMID=$(vms_with_tag "$SYSTEM_POOL" "$TAG_GATEWAY" | head -n 1)
	[[ -n $GATEWAY_VMID ]] || die "the install has no gateway VM; uninstall and install again"
	wait_agent "$CONTROLLER_VMID"
	load_installed_settings

	local new_gateway=0
	vm_has_tag "$GATEWAY_VMID" "$(release_tag)" || new_gateway=1
	step "Plan"
	say "Upgrade proxmox-actions-runners $release to $PAR_VERSION:"
	say "  import the $PAR_VERSION runner template unless it exists; the controller removes old ones once unused"
	((new_gateway == 0)) ||
		say "  replace gateway VM $GATEWAY_VMID with the new image (workers lose their network for a minute or two)"
	[[ $release == "$PAR_VERSION" ]] || say "  replace the parcon binary in controller VM $CONTROLLER_VMID and restart it"
	((DRY_RUN)) && return 0
	require_release
	confirm "Upgrade with this plan?" || die "cancelled"

	step "Download and verify images"
	make_work_dir
	download "par-runner-$PAR_VERSION.qcow2"
	download "par-runner-$PAR_VERSION.json"
	((new_gateway == 0)) || download "par-gateway-$PAR_VERSION.qcow2"
	import_template
	((new_gateway == 0)) || replace_gateway
	[[ $release == "$PAR_VERSION" ]] || upgrade_controller
	step "Done"
	say "proxmox-actions-runners is at $PAR_VERSION."
}

# ---------------------------------------------------------------------------------------------------------------
# Uninstall

# managed_vms prints the VMs tagged par-managed in our pools: clones before templates, since Proxmox won't destroy a
# template that linked clones still use.
managed_vms() {
	pvesh get /cluster/resources --type vm --output-format json | json '
		my %pools = map { $_ => 1 } @ARGV;
		my @vms = grep {
			$pools{$_->{pool} // ""} && grep { $_ eq "par-managed" } split /[;,]/, $_->{tags} // ""
		} @$d;
		print "$_->{vmid}\n"
			for sort { ($a->{template} // 0) <=> ($b->{template} // 0) || $a->{vmid} <=> $b->{vmid} } @vms;
	' "$SYSTEM_POOL" "$RUNNER_POOL"
}

# destroy_vm stops and destroys a VM. One that is already gone counts as destroyed.
destroy_vm() {
	local vmid=$1
	if qm status "$vmid" 2>/dev/null | grep -q running; then
		change qm stop "$vmid" --skiplock 1 || true
	fi
	change qm destroy "$vmid" --purge 1 && return 0
	! qm status "$vmid" >/dev/null 2>&1 || die "destroying VM $vmid failed"
}

# user_acls prints "path type ugid role" for every ACL of our user and its tokens.
user_acls() {
	pveum acl list --output-format json | json '
		for my $acl (@$d) {
			next unless $acl->{ugid} eq $ARGV[0] || index($acl->{ugid}, "$ARGV[0]!") == 0;
			print join(" ", @{$acl}{qw(path type ugid roleid)}), "\n";
		}' "$PVE_USER"
}

do_uninstall() {
	check_root >/dev/null || die "run it as root"
	local vms controller
	vms=$(managed_vms | tr '\n' ' ')
	controller=$(installed_controller)
	step "Plan"
	say "Remove proxmox-actions-runners from node $(node_name):"
	[[ -z $controller ]] || say "  stop the controller and delete its scale sets in GitHub (the App itself stays)"
	say "  destroy the VMs tagged $TAG_MANAGED in $SYSTEM_POOL and $RUNNER_POOL, now: ${vms:-none}"
	say "  remove pools $SYSTEM_POOL and $RUNNER_POOL, role $ROLE, user $PVE_USER with its token and ACLs"
	say "  remove SDN VNet $VNET and zone $ZONE, then apply the SDN config"
	((DRY_RUN)) && return 0
	confirm "Uninstall with this plan? This can't be undone." || die "cancelled"

	if [[ -n $controller ]] && qm status "$controller" | grep -q running; then
		step "Stop the controller"
		guest_exec "$controller" 60 -- systemctl disable --now parcon.service >/dev/null ||
			warn "stopping parcon.service failed"
		as_parcon "$controller" 120 parcon github scaleset delete ||
			warn "deleting the scale set in GitHub failed; remove it in the organization's or repository's" \
				"Actions settings (Runners)"
	fi

	step "Destroy VMs"
	# Listed again: until it stopped, the controller kept creating and destroying workers.
	local vmid
	for vmid in $(managed_vms); do
		destroy_vm "$vmid"
	done

	step "Proxmox access objects"
	local pool
	for pool in "$SYSTEM_POOL" "$RUNNER_POOL"; do
		exists_pool "$pool" || continue
		change pveum pool delete "$pool" || warn "pool $pool isn't empty: it holds VMs the installer didn't create"
	done
	if exists_user "$PVE_USER"; then
		local path type ugid role
		while read -r path type ugid role; do
			change pveum acl delete "$path" "--${type}s" "$ugid" --roles "$role"
		done < <(user_acls)
		if exists_token; then
			change pveum user token remove "$PVE_USER" "$TOKEN"
		fi
		change pveum user delete "$PVE_USER"
	fi
	if exists_role "$ROLE"; then
		change pveum role delete "$ROLE"
	fi

	step "Worker network"
	local changed=0
	if exists_sdn "vnets/$VNET"; then
		change pvesh delete "/cluster/sdn/vnets/$VNET"
		changed=1
	fi
	if exists_sdn "zones/$ZONE"; then
		change pvesh delete "/cluster/sdn/zones/$ZONE"
		changed=1
	fi
	((changed == 0)) || change pvesh set /cluster/sdn
	step "Done"
	say "proxmox-actions-runners is removed. The GitHub App remains; delete it in GitHub's settings if you like."
}

# ---------------------------------------------------------------------------------------------------------------

do_check() {
	[[ -z $ANSWERS ]] || load_answers "$ANSWERS"
	apply_defaults
	# The checks don't use the GitHub target.
	[[ -n $PAR_GITHUB_URL ]] || PAR_GITHUB_URL=https://github.com/example
	validate_settings
	preflight
	say "All checks passed."
}

main() {
	parse_args "$@"
	case $COMMAND in
	install) do_install ;;
	upgrade) do_upgrade ;;
	uninstall) do_uninstall ;;
	check) do_check ;;
	esac
}

# Tests source this file with PAR_INSTALL_SOURCED set. Otherwise main runs only once the whole script has arrived, so
# a download cut short can't run half of it.
[[ -n ${PAR_INSTALL_SOURCED:-} ]] || main "$@"
