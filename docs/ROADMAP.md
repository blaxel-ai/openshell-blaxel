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

- **Upstream: IPv6 policy DNS.** OpenShell main hardcodes `ipv6_egress = false` in the policy DNS path. That breaks allowed hosts on IPv6-only networks ([#2712](https://github.com/NVIDIA/OpenShell/issues/2712) touches the same area).
  - Branch [`Joffref/OpenShell:feat/policy-dns-ipv6-egress`](https://github.com/Joffref/OpenShell/tree/feat/policy-dns-ipv6-egress) adds `--policy-dns-ipv6-egress auto|enabled|disabled`, with `auto` meaning "IPv6 default route and no IPv4 default route".
  - It was tested live on Blaxel (`experiments/ipv6-egress-live.sh`):
    - IPv6-only: allowed host 200, L7 deny 403;
    - dual-stack: unchanged.
  - It will be filed with NVIDIA once the contributor vouch is in place. After it merges, the CLAT can go.

## Next

- Validate Claude Code end to end on main with a real API key ([CLAUDE-CODE.md](CLAUDE-CODE.md)).
- Move from the rolling `dev` release to a tagged OpenShell release once one ships with the RFC 0012 contract.
- Upstream: `is_internal_ip` should look inside NAT64 prefixes (`64:ff9b::/96` and the local prefix) before IPv6 answers are enabled by default.
- Survive control-plane redeploys without stopping running sandboxes. That needs the supervisor to re-attach, which main's contract doesn't allow yet.
