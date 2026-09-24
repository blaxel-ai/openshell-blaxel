import { SandboxInstance } from "@blaxel/core";
import fs from "node:fs";
const sb = await SandboxInstance.createIfNotExists({
  name: "os-exp-grpc", image: "blaxel/base-image:latest", memory: 2048, region: "us-was-1",
  ports: [{ target: 10000, protocol: "HTTP" }],
});
await sb.fs.writeBinary("/usr/local/bin/interop-server", fs.readFileSync("grpctest/interop-server"));
await sb.process.exec({ command: "chmod 755 /usr/local/bin/interop-server", waitForCompletion: true });
await sb.process.exec({ name: "grpc", command: "interop-server -port 10000", waitForPorts: [10000] });
const pub = await sb.previews.createIfNotExists({ metadata: { name: "grpc-public" }, spec: { port: 10000, public: true } });
const priv = await sb.previews.createIfNotExists({ metadata: { name: "grpc-private" }, spec: { port: 10000, public: false } });
const tok = await priv.tokens.create(new Date(Date.now() + 3600e3));
console.log(JSON.stringify({ pub: pub.spec.url, priv: priv.spec.url, tok: tok.value }));
