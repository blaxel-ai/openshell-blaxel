import { SandboxInstance } from "@blaxel/core";
const sb = await SandboxInstance.get("os-exp-tunnel");
const priv = await sb.previews.createIfNotExists({ metadata: { name: "tunnel-private" }, spec: { port: 9000, public: false } });
const tok = await priv.tokens.create(new Date(Date.now() + 3600e3));
console.log(JSON.stringify({ url: priv.spec.url, tok: tok.value }));
