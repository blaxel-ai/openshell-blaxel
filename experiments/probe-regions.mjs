// Kernel + Landlock availability per Blaxel region.
import { SandboxInstance } from "@blaxel/core";
const regions = process.argv.slice(2);
await Promise.all(regions.map(async (region) => {
  const name = `os-kprobe-${region.replace(/[^a-z0-9]/g, "")}`;
  try {
    const sb = await SandboxInstance.create({ name, image: "blaxel/base-image:latest", memory: 1024, region });
    const p = await sb.process.exec({ command: `uname -r; python3 -c 'import ctypes;print(ctypes.CDLL(None).syscall(444,0,0,1))' 2>/dev/null || (cat /sys/kernel/security/lsm 2>/dev/null; echo n/a)`, waitForCompletion: true, timeout: 30 });
    console.log(region.padEnd(12), (p.logs || "").trim().replace(/\n/g, " | "));
    await SandboxInstance.delete(name);
  } catch (e) { console.log(region.padEnd(12), "ERR", e.message.slice(0, 120)); }
}));
