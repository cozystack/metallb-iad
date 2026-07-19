# metallb-iad

MetalLB IP Allocation Driver — the reference per-class driver for the
[address-controller](https://github.com/lllamnyp/address-controller) core
(IP addresses as a first-class resource,
[cozystack/community#35](https://github.com/cozystack/community/pull/35)).

Like a CSI driver for a storage provider, this is the driver for one address
backend: it reserves and attaches IPs that are ultimately announced by
MetalLB.
