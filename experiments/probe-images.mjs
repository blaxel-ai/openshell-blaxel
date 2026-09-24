import { SandboxInstance } from "@blaxel/core";
for (const image of ["blaxel/py-app:latest", "blaxel/node:latest"]) {
  const name = "os-exp-img-" + image.split(/[/:]/)[1].replace(/[^a-z]/g, "");
  try {
    const sb = await SandboxInstance.createIfNotExists({ name, image, memory: 2048, region: "us-was-1" });
    const r = await sb.process.exec({ command: "head -2 /etc/os-release; ldd --version 2>&1 | head -1", waitForCompletion: true, timeout: 30 });
    console.log(image, "=>", r.logs.replace(/\n/g, " | "));
    await sb.delete();
  } catch (e) { console.log(image, "ERR", e.message); }
}
