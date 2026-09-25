// main runtime inside an empty netns on the landlock kernel.
import { SandboxInstance } from "@blaxel/core";
import fs from "node:fs";
const name = "os-wl-probe";
await SandboxInstance.delete(name).catch(() => {});
await new Promise((r) => setTimeout(r, 3000));
const sb = await SandboxInstance.create({ name, image: "blaxel/py-app:latest", memory: 2048, region: "us-was-1", extraArgs: { landlock: "enabled" } });
const sh = async (c, t = 55) => (await sb.process.exec({ command: c, waitForCompletion: true, timeout: t })).logs.trim();
for (let i = 0; i < 20; i++) { try { await sh("true"); break; } catch { await new Promise((r) => setTimeout(r, 3000)); } }
await sb.fs.writeBinary("/opt/os/openshell-sandbox", fs.readFileSync(process.argv[2]));
console.log(await sh("chmod 755 /opt/os/openshell-sandbox; uname -r; command -v ip unshare nsenter; id sandbox 2>/dev/null || useradd -m -u 1500 -s /bin/sh sandbox; echo user-ok"));
console.log("--- qualification inside an empty netns");
console.log(await sh(`unshare -n sh -c 'ip link set lo up 2>&1 || echo NO-IP; echo 0 > /proc/sys/net/ipv4/ip_unprivileged_port_start; ip -br addr 2>/dev/null; cd /tmp; /opt/os/openshell-sandbox capability-probe-launch 1500 1500 2>&1 | tail -5'`));
