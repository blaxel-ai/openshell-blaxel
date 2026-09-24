// Print every HTTP request the Blaxel SDK makes (headers redacted, bodies truncated).
const orig = globalThis.fetch;
globalThis.fetch = async (input, init = {}) => {
  const req = input instanceof Request ? input : new Request(input, init);
  const body = init.body ?? (input instanceof Request ? await input.clone().text().catch(() => "") : "");
  const hdrs = [...req.headers.keys()].filter((k) => !/authorization|cookie/i.test(k)).map((k) => `${k}=${req.headers.get(k)}`).join(" ");
  const shown = typeof body === "string" ? body.slice(0, 400) : `<${body?.constructor?.name}>`;
  const res = await orig(input, init);
  console.log(`>> ${req.method} ${req.url}\n   hdrs: ${hdrs}\n   body: ${shown}\n   << ${res.status}`);
  return res;
};
const { SandboxInstance } = await import("@blaxel/core");
const sb = await SandboxInstance.create({ name: "os-exp-trace", image: "blaxel/py-app:latest", memory: 2048, region: "us-was-1", ports: [{ target: 9000, protocol: "HTTP" }], labels: { "openshell.ai/sandbox-id": "abc" }, envs: [{ name: "FOO", value: "bar" }] });
await sb.fs.write("/tmp/a.txt", "hello");
await sb.fs.writeBinary("/tmp/b.bin", Buffer.from([1, 2, 3]));
await sb.process.exec({ name: "p1", command: "echo hi", waitForCompletion: true, timeout: 10 });
await sb.process.exec({ name: "bg", command: "sleep 100", keepAlive: true });
await sb.process.get("bg");
await sb.process.kill("bg");
await SandboxInstance.get("os-exp-trace");
await SandboxInstance.list();
await SandboxInstance.delete("os-exp-trace");
