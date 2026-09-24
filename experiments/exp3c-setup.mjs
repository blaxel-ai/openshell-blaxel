import { SandboxInstance } from "@blaxel/core";
import fs from "node:fs";
const sb = await SandboxInstance.createIfNotExists({
  name: "os-exp-gw", image: "blaxel/py-app:latest", memory: 4096, region: "us-was-1",
  ports: [{ target: 9000, protocol: "HTTP" }],
});
for (const [src, dst] of [["bin/openshell-gateway", "/usr/local/bin/openshell-gateway"], ["./wsrelay/wsrelay-linux-amd64", "/usr/local/bin/wsrelay"]])
  await sb.fs.writeBinary(dst, fs.readFileSync(src));
const r = await sb.process.exec({ command: "chmod 755 /usr/local/bin/*; (apt-get update -qq && DEBIAN_FRONTEND=noninteractive apt-get install -y -qq podman iproute2 >/dev/null 2>&1; echo apt=$?); openshell-gateway --version 2>&1 | head -3; podman --version", waitForCompletion: true, timeout: 60 });
console.log(r.logs);
