import { SandboxInstance } from "@blaxel/core";
const sb = await SandboxInstance.get(process.argv[2]);
const p = await sb.process.exec({ command: process.argv[3], waitForCompletion: true, timeout: 55 }).catch(e => ({ logs: "ERR " + e.message }));
console.log((p.logs ?? "").trim(), p.exitCode !== undefined ? `\n[exit ${p.exitCode}]` : "");
