// Fresh landlock-variant sandbox: wait until the sandbox API answers, then
// report kernel, Landlock ABI, and OpenShell main qualification.
import { SandboxInstance } from "@blaxel/core";
import fs from "node:fs";
const [name, sandboxBin, probeBin] = process.argv.slice(2);
await SandboxInstance.delete(name).catch(() => {});
await new Promise((r) => setTimeout(r, 4000));
const t0 = Date.now();
const sb = await SandboxInstance.create({ name, image: "blaxel/base-image:latest", memory: 2048, region: "us-was-1", extraArgs: { landlock: "enabled" } });
const sh = async (c) => (await sb.process.exec({ command: c, waitForCompletion: true, timeout: 50 })).logs.trim();
let up = false;
for (let i = 0; i < 24 && !up; i++) {
  try { await sh("true"); up = true; } catch { await new Promise((r) => setTimeout(r, 5000)); }
}
if (!up) { console.log(`NOT REACHABLE after ${((Date.now() - t0) / 1000).toFixed(0)}s`); process.exit(1); }
console.log(`reachable after ${((Date.now() - t0) / 1000).toFixed(1)}s`);
console.log(await sh("uname -r; mount -t securityfs securityfs /sys/kernel/security 2>/dev/null; echo lsm=$(cat /sys/kernel/security/lsm); zcat /proc/config.gz 2>/dev/null | grep -E '^(# )?CONFIG_(SECURITY_LANDLOCK|LSM)[= ]'"));
await sb.fs.writeBinary("/tmp/probe", fs.readFileSync(probeBin));
await sb.fs.writeBinary("/tmp/os-main-sandbox", fs.readFileSync(sandboxBin));
console.log(await sh("chmod 755 /tmp/probe /tmp/os-main-sandbox; timeout 25 /tmp/probe 2>&1 | grep -E 'landlock|seccomp'"));
console.log("--- capability-probe-launch 1500 1500");
console.log(await sh("cd /tmp; timeout 40 /tmp/os-main-sandbox capability-probe-launch 1500 1500 2>&1 | tail -40"));
