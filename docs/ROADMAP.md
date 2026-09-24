# Roadmap: OpenShell `main`

This repository implements the **v0.0.116** compute-driver contract. OpenShell
`main` (RFC 0012, PR #3366) changes the contract substantially, and the current
driver won't work there.

## What changes on `main`

- `DriverSandboxSpec.launch_authentication`: a gateway-minted bundle the driver
  splits between the supervisor (JWTs) and the sandbox (verification keys). The
  sandbox receives no gateway JWT.
- `openshell-supervisor` runs **outside** the workload VM, on a trusted host,
  like the VM driver. It talks to `openshell-sandbox` over a TLS 1.3 boundary
  channel (`BoundaryListener::TlsTcp`), which our WS tunnel can carry.
- `openshell-sandbox launch-capability-free <uid> <gid> <bootstrap>` starts as root,
  drops to zero capabilities, and hard-requires Landlock ABI ≥ 3.
- The driver asserts outer-fence guarantees (`DefaultDenyEgress`,
  `NoUnmanagedEgressPath`, `RevocationVerified`, `ControllerLossFailsClosed`),
  which the empty-netns design can back honestly.
- `GetCapabilities` negotiates extension metadata (`openshell.compute.contract`)
  and resource-admission policy.

## Status

- Kernel: **done**. The `landlock` variant qualifies (`qualified: true`, ABI v7),
  see [KERNEL.md](KERNEL.md).
- Driver: to write in **Rust** inside the OpenShell workspace. The `main` wire
  formats are serde structs with `deny_unknown_fields`, built by
  `openshell-sandbox-backend` helpers. It reuses the Kubernetes TlsTcp boundary
  code and the VM host-supervisor spawn. This repo's tunnel and Blaxel client
  designs carry over.
- Upstream: an issue on NVIDIA/OpenShell describing the Blaxel placement is to be drafted,
  following their template.
