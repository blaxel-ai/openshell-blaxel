// Experiment 1: can openshell-sandbox's kernel requirements be met in a Blaxel VM?
import { SandboxInstance } from "@blaxel/core";
import fs from "node:fs";

const name = "os-exp-kernel";
const sb = await SandboxInstance.createIfNotExists({
  name,
  image: "blaxel/base-image:latest",
  memory: 2048,
  region: "us-was-1",
});
console.log("sandbox:", sb.metadata.url);

const run = async (command) => {
  const p = await sb.process.exec({ command, waitForCompletion: true, timeout: 60 });
  return (p.logs ?? "").trim();
};

const arch = await run("uname -m");
const bin = arch === "aarch64" ? "probe-arm64" : "probe-amd64";
await sb.fs.writeBinary("/tmp/probe", fs.readFileSync(new URL(`./probe/${bin}`, import.meta.url)));
await run("chmod 755 /tmp/probe");

console.log("--- as root");
console.log(await run("/tmp/probe"));
console.log("--- as nobody (65534)");
console.log(await run("command -v setpriv >/dev/null && setpriv --reuid=65534 --regid=65534 --clear-groups /tmp/probe || su -s /bin/sh nobody -c /tmp/probe"));
console.log("--- env");
console.log(await run("cat /etc/os-release | head -2; ps -o pid,user,comm | head -5; ip -br addr 2>/dev/null || cat /proc/net/dev"));
