# OpenShell on Blaxel — experiment results (2026-09-23)

Workspace `charlou-dev`, env `dev`, region `us-was-1`, OpenShell v0.0.116 release binaries.

| # | Question | Result |
|---|---|---|
| 1 | Guest kernel features | Linux 6.1.166 x86_64. seccomp user-notif OK (root + unprivileged). netns OK (root + unprivileged userns), empty netns has only `lo`, egress blocked, Unix socket across netns OK. **Landlock: ENOSYS** (not compiled in). |
| 1b | Real `openshell-sandbox` v0.0.116 | Runs as root (needs iproute2 + a `sandbox` user). Creates netns + veth + proxy, denies direct egress, enforces OPA policy. Emits HIGH "Landlock Filesystem Sandbox Unavailable" and continues (`landlock.compatibility: best_effort`). |
| 1c | OpenShell `main` (679b190) | Built for x86_64-musl and run on Blaxel. `capability-probe-launch 1500 1500` fails: `fully incompatible access-rights` → `Landlock allow/deny probe` exit 1. Other requirements checked by hand: seccomp user-notif OK, no Yama (same-uid `/proc/<pid>/mem` read OK), `ip_unprivileged_port_start` settable to 0 as root. **Landlock is the only blocker found.** |
| 2 | Raw stream over WS via `/port/N` | Works. RTT p50 ~70 ms, ~20 MiB/s, 8 concurrent streams, 8 MiB random bytes echoed intact. Idle WS without pings dies before 300 s; with 20 s WS pings it survived 600 s idle. Throughput varied between runs (2.8–22 MiB/s). |
| 3a | Native gRPC through preview URL | **Fails**: preview proxy talks HTTP/1.1 to the workload → `502 UPSTREAM_ERROR`. `/port/N` adds a path prefix gRPC can't use. |
| 3b | Real gateway in Blaxel, CLI on Mac | Works through the WS tunnel: `Connected, Authenticated (mTLS transport)`, ~0.5 s per call. mTLS survives because it is end-to-end inside the tunnel. |
| 3c | Nested Podman sandbox in gateway VM | Fails on disk: image unpack fills tmpfs (50% of RAM). Needs a volume; not on the main path. |
| 4 | Scoped auth for tunnels | Private preview: 401 without token; preview token works as `X-Blaxel-Preview-Token` header or `bl_preview_token` query. |

Gotchas found: `openshell gateway add --local` overwrites the gateway's `mtls/` dir with the Homebrew certs;
Go `RawSyscall` while seccomp-blocked deadlocks the runtime (use `Syscall`); Alpine `gcompat` can't run the
glibc gateway (use `blaxel/py-app`, Debian 13).

## 2026-09-24: Landlock kernel variant (charlou-dev, us-was-1 dev)

Sandbox created with `spec.runtime.extraArgs: {landlock: "enabled"}` boots `vmlinux-landlock` (Linux 6.18.25)
in 1.6 s. `lsm=capability,landlock`, Landlock ABI v7.

OpenShell `main` (679b190) `openshell-sandbox capability-probe-launch 1500 1500`, after setting
`net.ipv4.ip_unprivileged_port_start=0` as root (the driver's job, as in the VM guest init):

```
{"qualified":true,"uid":1500,"gid":1500,"capabilities_zero":true,"no_new_privileges":true,
 "landlock_abi":7,"landlock_allow_deny":true,"seccomp_notification":true,"seccomp_addfd_send":true,
 "task_memory_copy":true,"socket_virtualization":true,"dns_relay_bind":true,"udp_dns_round_trip":true,
 "tcp_dns_round_trip":true,"tcp_allow_round_trip":true,"tcp_deny_round_trip":true,
 "wait_killable_recv":true,"seccomp_listener_mode":"killable"}
```

History of the variant: first build was 6.1 (ABI v2, fails on Truncate); second build 6.18 panicked
at boot (`FIPS140 loader: module loading error`, no modules in Firecracker boot); current build boots.
