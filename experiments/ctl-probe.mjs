import { SandboxInstance } from "@blaxel/core";
import fs from "node:fs";
const name = "os-control";
const sb = await SandboxInstance.createIfNotExists({ name, image: "blaxel/py-app:latest", memory: 4096, region: "us-was-1", ports: [{ target: 9000, protocol: "HTTP" }] });
const sh = async (c, t = 55) => (await sb.process.exec({ command: c, waitForCompletion: true, timeout: t })).logs.trim();
for (let i = 0; i < 20; i++) { try { await sh("true"); break; } catch { await new Promise((r) => setTimeout(r, 3000)); } }
for (const b of ["openshell-gateway", "openshell-supervisor"]) await sb.fs.writeBinary(`/opt/os/bin/${b}`, fs.readFileSync(`../bin/main/${b}`));
console.log(await sh("chmod 755 /opt/os/bin/*; uname -r; ldd --version | head -1; /opt/os/bin/openshell-gateway --version; /opt/os/bin/openshell-supervisor --version 2>&1 | head -2; ldd /opt/os/bin/openshell-gateway | grep -c 'not found'"));
console.log("--- supervisor --help"); console.log(await sh("/opt/os/bin/openshell-supervisor --help 2>&1 | head -70"));
