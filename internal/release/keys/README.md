# Release signing keys

`parcon` embeds every `*.pem` file here and accepts a release only if one of these Ed25519 public keys verifies
its `SHA256SUMS.sig`. The release workflow signs with the private key in the `RELEASE_SIGNING_KEY` secret of the
`release` environment, and checks the signature against these files before it publishes anything.

See *Release signing* in [AGENTS.md](../../../AGENTS.md#releases-and-ci-github) for how to create, rotate, and
recover a key. Only public keys belong here; the private key never goes in the repository.
