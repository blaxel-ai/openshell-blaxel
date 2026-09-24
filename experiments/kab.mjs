// A/B: same image and region, default kernel vs landlock variant.
import { SandboxInstance } from "@blaxel/core";
const suffix = Date.now().toString(36).slice(-5);
const run = async (label, extra) => {
  const name = `os-k${label}-${suffix}`, t0 = Date.now();
  try {
    const sb = await SandboxInstance.create({ name, image: "blaxel/base-image:latest", memory: 2048, region: "us-was-1", ...(extra && { extraArgs: extra }) });
    const got = (await SandboxInstance.get(name)).spec?.runtime?.extraArgs ?? null;
    for (let i = 0; i < 30; i++) {
      try {
        const p = await sb.process.exec({ command: "uname -r; mount -t securityfs securityfs /sys/kernel/security 2>/dev/null; cat /sys/kernel/security/lsm", waitForCompletion: true, timeout: 20 });
        return console.log(label.padEnd(9), name, "extraArgs=" + JSON.stringify(got), `up ${((Date.now() - t0) / 1000).toFixed(1)}s:`, p.logs.trim().replace(/\n/g, " lsm="));
      } catch { await new Promise((r) => setTimeout(r, 5000)); }
    }
    console.log(label.padEnd(9), name, "extraArgs=" + JSON.stringify(got), "NOT REACHABLE after", ((Date.now() - t0) / 1000).toFixed(0) + "s");
  } catch (e) { console.log(label, "create ERR", (e.message || JSON.stringify(e)).slice(0, 200)); }
};
await Promise.all([run("default"), run("landlock", { landlock: "enabled" })]);
