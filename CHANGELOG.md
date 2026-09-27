# Changelog

## [0.2.1](https://github.com/klponce/proxmox-actions-runners/compare/v0.2.0...v0.2.1) (2026-09-27)


### Bug fixes

* **controller:** don't let a slow GitHub stall cleanup and new workers ([905c63c](https://github.com/klponce/proxmox-actions-runners/commit/905c63c781730c888ec626ad484029a445af4c67))
* **controller:** don't let a slow GitHub stall cleanup and new workers ([49fe079](https://github.com/klponce/proxmox-actions-runners/commit/49fe079e93daa904ac4d1dd12c6d3e8a101936c9))

## [0.2.0](https://github.com/klponce/proxmox-actions-runners/compare/v0.1.1...v0.2.0) (2026-09-27)


### Features

* **config:** make a worker's maximum lifetime configurable ([1458044](https://github.com/klponce/proxmox-actions-runners/commit/1458044ab0ca0477867748e3e0d0625fbab792f2))
* **config:** make a worker's maximum lifetime configurable ([37a5cc9](https://github.com/klponce/proxmox-actions-runners/commit/37a5cc95b9d373fe8ec1e7e431613f3486b3d46f))

## [0.1.1](https://github.com/klponce/proxmox-actions-runners/compare/v0.1.0...v0.1.1) (2026-09-26)


### Bug fixes

* **config:** warn only about the key being set ([38d09dc](https://github.com/klponce/proxmox-actions-runners/commit/38d09dc974d9d87b21f5a9a3198d9a9056ea1884))
* install link last, status progress, config set warnings ([7e258a3](https://github.com/klponce/proxmox-actions-runners/commit/7e258a306fefa8781b34866ebfe43bcbba73be58))
* **install:** end with the App's install link while waiting for it ([1d15710](https://github.com/klponce/proxmox-actions-runners/commit/1d15710e766be121092225554c5e86ae97244be5))
* **status:** say at once that it is collecting, and report each part as it comes in ([de7ec7d](https://github.com/klponce/proxmox-actions-runners/commit/de7ec7d20966c3b63df11219df2fd47041bd34d6))

## 0.1.0 (2026-09-26)


### Features

* foundations for parcon on the Proxmox host ([ea89246](https://github.com/klponce/proxmox-actions-runners/commit/ea8924689eb85c0ab499ec3cdffc7e1b3c4721c2))
* **install:** install.sh becomes a bootstrap for parcon ([2f8c2b1](https://github.com/klponce/proxmox-actions-runners/commit/2f8c2b12f10c9dc7b162b5dcc704daa747c74497))
* **install:** turn off the LAN NIC's offloads on a node that is itself a VM ([496716b](https://github.com/klponce/proxmox-actions-runners/commit/496716b01d5737babe234598d3420fb81f88ba05))
* parcon install, check, and uninstall on the Proxmox host ([f643507](https://github.com/klponce/proxmox-actions-runners/commit/f6435070ace8ca9254a5e03628d822279b04f372))
* parcon on the Proxmox host, signed releases with release-please ([fdd21c9](https://github.com/klponce/proxmox-actions-runners/commit/fdd21c92c2a1d060365bdb06d713575fdde7375e))
* parcon status and parcon config on the Proxmox host ([5222f2f](https://github.com/klponce/proxmox-actions-runners/commit/5222f2f813ee76d77ca4a5e611a1cce20659943a))
* parcon update ([9a71558](https://github.com/klponce/proxmox-actions-runners/commit/9a715587b9ea2b6210d7b631b5a40a2653444c84))
* **release:** signed SHA256SUMS and the code to find and verify releases ([eb54ad3](https://github.com/klponce/proxmox-actions-runners/commit/eb54ad31a59270bb379ca8abe1776c28aeba492e))
* **release:** trust the 2026-1 release signing key ([054945e](https://github.com/klponce/proxmox-actions-runners/commit/054945e34d59739108a48e4eff38d80495d3223d))
* **settings:** host settings, the controller config rendered from them, and config keys ([312758e](https://github.com/klponce/proxmox-actions-runners/commit/312758e9f2da775786251ab135128475f5b3c141))


### Bug fixes

* **pvecli:** take a guest command's status even when qm exits non-zero ([bf708ae](https://github.com/klponce/proxmox-actions-runners/commit/bf708ae2752663b030ae9531c4a9a2646d630957))


### Documentation

* commits and releases with release-please ([6ad8e72](https://github.com/klponce/proxmox-actions-runners/commit/6ad8e727196bf9db14278c7a295409984e33c511))
* parcon on the host, host settings, and signed releases ([47ba2b2](https://github.com/klponce/proxmox-actions-runners/commit/47ba2b2df5ecb6dd3b6abad7498ae8502008700a))
