# Roadmap

## Done

- **OpenShell main port.** This driver implements the main compute-driver contract (RFC 0012, PR #3366):
  - `launch_authentication` split between the supervisor and the sandbox;
  - per-session TLS boundary;
  - outer-fence guarantees backed by a loopback-only netns;
  - extension negotiation and admission acknowledgement;
  - gateway-owned readiness.

  It's written in Go against the copied protos, and its wire formats are checked against the real binaries.
- **Everything on Blaxel.** Gateway, driver and supervisors run in a control sandbox, deployed by `os-deploy`. The laptop only runs the CLI.
- **Kernel.** The `landlock` variant qualifies (`qualified: true`, ABI v7), see [KERNEL.md](KERNEL.md).
- **IPv4 for the supervisor.** A CLAT (tayga) and a DoH forwarder in the control sandbox work around Blaxel's IPv6-only network.

## In progress

- **Upstream: IPv6 policy DNS and NAT64 SSRF checks.** OpenShell main hardcodes `ipv6_egress = false` in the policy DNS path, which breaks allowed hosts on IPv6-only networks, and its SSRF checks don't look inside NAT64 prefixes.
  - [NVIDIA/OpenShell#3702](https://github.com/NVIDIA/OpenShell/pull/3702), in review, tracked by issue [#3716](https://github.com/NVIDIA/OpenShell/issues/3716). It adds:
    - `policy_dns_ipv6_egress = auto|enabled|disabled` in the driver configs;
    - `nat64_prefixes` and `ipv4only.arpa` discovery;
    - NAT64-aware SSRF checks, and OCSF events for the decision.
  - The first version was tested live on Blaxel (`experiments/ipv6-egress-live.sh`):
    - IPv6-only: allowed host 200, L7 deny 403;
    - dual-stack: unchanged.
  - Still open in #3716: a sandbox-side IPv6 path test and an IPv6-only CI lane. Blaxel's `landlock` sandboxes are IPv6-only and could host both.
  - Once it's merged, the driver can pass Blaxel's NAT64 prefix with `--nat64-prefix`, and the CLAT and DoH forwarder can go.

## Next

- Validate Claude Code end to end on main with a real API key ([CLAUDE-CODE.md](CLAUDE-CODE.md)).
- Move from the rolling `dev` release to a tagged OpenShell release once one ships with the RFC 0012 contract.
- Survive control-plane redeploys without stopping running sandboxes. That needs the supervisor to re-attach, which main's contract doesn't allow yet.
