import { SandboxInstance } from "@blaxel/core";
import fs from "node:fs";
const sb = await SandboxInstance.get("os-exp-grpc");
await sb.fs.writeBinary("/usr/local/bin/protoecho", fs.readFileSync("protoecho/protoecho"));
await sb.process.exec({ command: "chmod 755 /usr/local/bin/protoecho", waitForCompletion: true });
await sb.process.exec({ name: "protoecho", command: "protoecho", waitForPorts: [10001] });
const p = await sb.previews.createIfNotExists({ metadata: { name: "protoecho" }, spec: { port: 10001, public: true } });
console.log(p.spec.url);
