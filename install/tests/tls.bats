#!/usr/bin/env bats
# How the controller verifies the Proxmox API's certificate: against the node's CA, against the system CAs, or, for a
# certificate from a CA nobody trusts, by a pinned fingerprint. Uses real certificates made with openssl.

load helpers

# cert NAME SUBJECT [ISSUER]: a key and certificate in $BATS_TEST_TMPDIR, self-signed or signed by ISSUER, for
# localhost, pve1, pve1.example.com, and 192.0.2.5.
cert() {
	local name=$1 subject=$2 issuer=${3:-} dir=$BATS_TEST_TMPDIR
	openssl req -new -newkey rsa:2048 -nodes -keyout "$dir/$name.key" -subj "/CN=$subject" -out "$dir/$name.csr" \
		2>/dev/null
	if [[ -z $issuer ]]; then
		openssl x509 -req -in "$dir/$name.csr" -key "$dir/$name.key" -days 30 -out "$dir/$name.pem" \
			-extfile <(printf 'basicConstraints=critical,CA:TRUE\nkeyUsage=keyCertSign\n') 2>/dev/null
	else
		openssl x509 -req -in "$dir/$name.csr" -CA "$dir/$issuer.pem" -CAkey "$dir/$issuer.key" -days 30 \
			-out "$dir/$name.pem" \
			-extfile <(printf 'subjectAltName=DNS:localhost,DNS:pve1,DNS:pve1.example.com,IP:192.0.2.5\n') 2>/dev/null
	fi
}

setup_node_certs() {
	PVE_DIR=$BATS_TEST_TMPDIR/pve
	mkdir -p "$PVE_DIR/local"
	cert node-ca "Proxmox Virtual Environment"
	cert node pve1 node-ca
	cp "$BATS_TEST_TMPDIR/node-ca.pem" "$PVE_DIR/pve-root-ca.pem"
	cp "$BATS_TEST_TMPDIR/node.pem" "$PVE_DIR/local/pve-ssl.pem"
}

@test "the node's own certificate is verified against the node's CA" {
	setup_node_certs
	[ "$(tls_mode)" = ca ]
	[ "$(tls_server_name)" = pve1 ]
	[ "$(tls_config)" = "  caCertFile: /etc/proxmox-actions-runners/pve-ca.pem
  tlsServerName: pve1" ]
	run check_tls
	[ "$status" -eq 0 ]
}

@test "a certificate the system CAs trust needs no CA file" {
	setup_node_certs
	cert acme-ca "Some Public CA"
	cert acme pve1 acme-ca
	cp "$BATS_TEST_TMPDIR/acme.pem" "$PVE_DIR/local/pveproxy-ssl.pem"
	# Stand in for the system trust store.
	export SSL_CERT_FILE=$BATS_TEST_TMPDIR/acme-ca.pem
	[ "$(tls_mode)" = system ]
	[ "$(tls_config)" = "  tlsServerName: pve1" ]
	run check_tls
	[ "$status" -eq 0 ]
	[[ $output == *"verified as pve1"* ]]
}

@test "a certificate from a CA nobody trusts is pinned, with a warning" {
	setup_node_certs
	cert private-ca "Private CA"
	cert custom pve1 private-ca
	cp "$BATS_TEST_TMPDIR/custom.pem" "$PVE_DIR/local/pveproxy-ssl.pem"
	export SSL_CERT_FILE=/dev/null
	[ "$(tls_mode)" = pin ]
	[[ $(tls_config) =~ ^\ \ tlsFingerprint:\ \"([0-9A-F]{2}:){31}[0-9A-F]{2}\"$ ]]
	run check_tls
	[ "$status" -eq 1 ]
	[[ $output == *"needs its new fingerprint"* ]]
}
