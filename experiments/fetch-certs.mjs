import { SandboxInstance } from "@blaxel/core";
import fs from "node:fs";
const sb = await SandboxInstance.get("os-exp-gw");
const dir = `${process.env.HOME}/.config/openshell/gateways/blaxel-exp/mtls`;
fs.mkdirSync(dir, { recursive: true, mode: 0o700 });
for (const [src, dst, mode] of [["ca.crt", "ca.crt", 0o644], ["client/tls.crt", "tls.crt", 0o644], ["client/tls.key", "tls.key", 0o600]]) {
  fs.writeFileSync(`${dir}/${dst}`, await sb.fs.read(`/srv/os/tls/${src}`), { mode });
}
console.log("wrote", fs.readdirSync(dir));
