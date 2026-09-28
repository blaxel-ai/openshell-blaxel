# Kernel requirements

OpenShell enforces its filesystem policy with Landlock. OpenShell `main`
makes it mandatory, so the Blaxel guest kernel must provide it.

## Selecting the kernel

```json
{"spec": {"runtime": {"extraArgs": {"landlock": "enabled"}}}}
```

The driver sets this by default (`-kernel-variant landlock`). The variant is
fixed at sandbox creation.

The control sandbox doesn't run agents, so it uses a different variant (see
[Control sandbox](#control-sandbox)).

## Required kernel config

```
CONFIG_SECURITY=y
CONFIG_SECURITYFS=y
CONFIG_SECURITY_LANDLOCK=y
CONFIG_LSM="landlock,…"
CONFIG_SECCOMP=y
CONFIG_SECCOMP_FILTER=y
CONFIG_USER_NS=y
CONFIG_NET_NS=y
```

Plus kernel ≥ 6.2: OpenShell `main` needs Landlock ABI ≥ 3 (truncate). Firecracker
boots a bare `vmlinux` with no modules, so nothing required at boot may
be a module (see the FIPS140 note below).

## Qualification

`main`'s runtime carries its own check. In a fresh sandbox, as root:

```sh
echo 0 > /proc/sys/net/ipv4/ip_unprivileged_port_start   # the driver does this
openshell-sandbox capability-probe-launch 1500 1500
```

Must print `"qualified": true`. `experiments/kab.mjs` compares boot on the default
kernel with the variant; `experiments/kcheck2.mjs` runs the full check.

## Control sandbox

The control sandbox runs the gateway, the driver and the supervisors, and none
of them need Landlock. What it needs is IPv4 egress on an IPv6-only network,
through a CLAT: `tayga` needs a TUN device, and the translated traffic leaves
through an ip6tables `MASQUERADE` rule. Blaxel's default kernel has neither, so
`os-deploy` creates the control sandbox with:

```json
{"spec": {"runtime": {"extraArgs": {"tun": "enabled", "iptables": "enabled"}}}}
```

That boots Linux 6.1.166. The guest exposes no kernel config (no
`/proc/config.gz`), so check the features instead:

```sh
ls /dev/net/tun                              # TUN device
ip6tables -t nat -S POSTROUTING              # shows the CLAT MASQUERADE rule
ip -4 route                                  # default dev clat
```

Checked on `os-control` (us-was-1): `/dev/net/tun` present,
`-A POSTROUTING -s fd64:c1a7:0:1::/96 -o eth0 -j MASQUERADE`, and
`default dev clat`.

Once NVIDIA/OpenShell#3702 is merged, the supervisor can use IPv6 directly
and the control sandbox can go back to the default kernel.

| Sandbox | Variant | Kernel | Needs |
|---|---|---|---|
| Workload | `landlock` | 6.18.25 | Landlock ABI ≥ 3, seccomp, netns |
| Control | `tun` + `iptables` | 6.1.166 | TUN, ip6tables NAT |
| Default | none | 6.1.166 | none of the above |

## History (us-was-1)

| Build | Kernel | Result |
|---|---|---|
| default | 6.1.166 | no Landlock (`lsm=capability`) |
| landlock #1 | 6.1.166 | Landlock ABI v2: fails on `Truncate` |
| landlock #2 | 6.18.25 | panic at boot: `FIPS140 loader: module loading error` (FIPS140 built as a module, no modules in Firecracker boot) |
| landlock #3 | 6.18.25 | boots in 1.6 s, ABI v7, **`qualified: true`** |

For reference, eu-dub-1 runs 6.12.75 with
`# CONFIG_SECURITY_LANDLOCK is not set`.
