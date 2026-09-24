import { SandboxInstance } from "@blaxel/core";
const [name, image] = process.argv.slice(2);
await SandboxInstance.delete(name).catch(() => {});
await new Promise((r) => setTimeout(r, 3000));
const t0 = Date.now();
try { await SandboxInstance.create({ name, image, memory: 2048, region: "eu-dub-1" }); }
catch (e) { console.log("create returned:", e.message.slice(0, 100)); }
for (let i = 0; i < 60; i++) {
  const s = await SandboxInstance.get(name).catch((e) => ({ status: "GET-ERR " + e.message.slice(0, 60) }));
  const st = s.status ?? s.sandbox?.status;
  if (st === "DEPLOYED" || st === "FAILED") {
    console.log(image, st, `${((Date.now() - t0) / 1000).toFixed(0)}s`);
    if (st === "DEPLOYED") {
      const p = await s.process.exec({ command: "uname -r; cat /proc/version | cut -c1-120", waitForCompletion: true, timeout: 30 });
      console.log(p.logs.trim());
    }
    break;
  }
  await new Promise((r) => setTimeout(r, 5000));
}
