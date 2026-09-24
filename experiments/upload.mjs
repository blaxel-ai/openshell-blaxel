import { SandboxInstance } from "@blaxel/core";
import fs from "node:fs";
const sb = await SandboxInstance.get(process.argv[2]);
await sb.fs.writeBinary(process.argv[4], fs.readFileSync(process.argv[3]));
console.log("uploaded", process.argv[4]);
