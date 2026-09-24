import { SandboxInstance } from "@blaxel/core";
import fs from "node:fs";
const sb = await SandboxInstance.createIfNotExists({
  name: "os-exp-tunnel", image: "blaxel/base-image:latest", memory: 2048, region: "us-was-1",
  ports: [{ target: 9000, protocol: "HTTP" }],
});
console.log("url", sb.metadata.url);
await sb.fs.writeBinary("/usr/local/bin/wsrelay", fs.readFileSync("./wsrelay/wsrelay-linux-amd64"));
const ex = (o) => sb.process.exec(o);
await ex({ command: "chmod 755 /usr/local/bin/wsrelay", waitForCompletion: true });
await ex({ name: "echo", command: "wsrelay echo -listen unix:/tmp/echo.sock", keepAlive: false });
await ex({ name: "relay", command: "wsrelay serve -listen 0.0.0.0:9000 -target unix:/tmp/echo.sock", waitForPorts: [9000] });
const r = await sb.fetch(9000, "/healthz");
console.log("healthz via /port/9000:", r.status, await r.text());
