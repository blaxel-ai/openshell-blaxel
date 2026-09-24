// Fresh sandbox: kernel, Landlock config, and OpenShell main qualification.
import { SandboxInstance } from "@blaxel/core";
import fs from "node:fs";
const [name, region, sandboxBin, extra] = process.argv.slice(2);
const extraArgs = extra ? Object.fromEntries(extra.split(",").map((kv) => kv.split("="))) : undefined;
await SandboxInstance.delete(name).catch(() => {});
await new Promise((r) => setTimeout(r, 3000));
const sb = await SandboxInstance.create({ name, image: "blaxel/base-image:latest", memory: 2048, region, ...(extraArgs && { extraArgs }) });
console.log("extraArgs:", JSON.stringify(extraArgs ?? {}), "->", JSON.stringify((await SandboxInstance.get(name)).spec?.runtime?.extraArgs ?? null));
const sh = async (c) => (await sb.process.exec({ command: c, waitForCompletion: true, timeout: 50 })).logs.trim();
console.log(await sh("uname -r; zcat /proc/config.gz 2>/dev/null | grep -E '^(# )?CONFIG_(SECURITY_LANDLOCK|LSM)[= ]'; mount -t securityfs securityfs /sys/kernel/security 2>/dev/null; echo lsm=$(cat /sys/kernel/security/lsm)"));
await sb.fs.writeBinary("/tmp/os-main-sandbox", fs.readFileSync(sandboxBin));
console.log("--- capability-probe-launch 1500 1500");
console.log(await sh("chmod 755 /tmp/os-main-sandbox; cd /tmp; timeout 40 /tmp/os-main-sandbox capability-probe-launch 1500 1500 2>&1 | tail -40"));
