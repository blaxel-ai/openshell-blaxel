import { SandboxInstance } from "@blaxel/core";
await Promise.all(["android", "iptables", "nfs", "tun"].map(async (k) => {
  const name = `os-kvar-${k}`;
  try {
    await SandboxInstance.delete(name).catch(() => {});
    const sb = await SandboxInstance.create({ name, image: "blaxel/base-image:latest", memory: 1024, region: "us-was-1", extraArgs: { [k]: "enabled" } });
    const p = await sb.process.exec({ command: "uname -r; mount -t securityfs securityfs /sys/kernel/security 2>/dev/null; echo lsm=$(cat /sys/kernel/security/lsm); zcat /proc/config.gz 2>/dev/null | grep -E '^(# )?CONFIG_SECURITY_LANDLOCK[= ]' || echo no-config.gz", waitForCompletion: true, timeout: 30 });
    console.log(k.padEnd(9), p.logs.trim().replace(/\n/g, "  "));
  } catch (e) { console.log(k.padEnd(9), "ERR", (e.message || JSON.stringify(e)).slice(0, 140)); }
  await SandboxInstance.delete(name).catch(() => {});
}));
