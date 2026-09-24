import { SandboxInstance } from "@blaxel/core";
const n = +process.argv[2];
await Promise.all([...Array(n).keys()].map(async (i) => {
  const name = `os-kspread-${i}`, image = i % 2 ? "blaxel/py-app:latest" : "blaxel/base-image:latest";
  try {
    const sb = await SandboxInstance.create({ name, image, memory: 1024, region: "us-was-1" });
    const p = await sb.process.exec({ command: "uname -r; mount -t securityfs securityfs /sys/kernel/security 2>/dev/null; cat /sys/kernel/security/lsm", waitForCompletion: true, timeout: 30 });
    console.log(name, image.padEnd(26), p.logs.trim().replace(/\n/g, "  lsm="));
  } catch (e) { console.log(name, "ERR", e.message.slice(0, 100)); }
  await SandboxInstance.delete(name).catch(() => {});
}));
