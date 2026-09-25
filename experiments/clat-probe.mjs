import { SandboxInstance } from "@blaxel/core";
const name = "os-clat-probe";
await SandboxInstance.delete(name).catch(() => {});
await new Promise((r) => setTimeout(r, 3000));
let sb;
try { sb = await SandboxInstance.create({ name, image: "blaxel/py-app:latest", memory: 2048, region: "us-was-1", extraArgs: { tun: "enabled", iptables: "enabled" } }); }
catch (e) { console.log("create ERR", (e.message || JSON.stringify(e)).slice(0, 300)); process.exit(0); }
const sh = async (c) => (await sb.process.exec({ command: c, waitForCompletion: true, timeout: 55 })).logs.trim();
for (let i = 0; i < 30; i++) { try { await sh("true"); break; } catch { await new Promise((r) => setTimeout(r, 3000)); } }
console.log("extraArgs:", JSON.stringify((await SandboxInstance.get(name)).spec?.runtime?.extraArgs));
console.log(await sh(`uname -r; ls -la /dev/net/tun 2>&1; apt-get update -qq >/dev/null 2>&1; DEBIAN_FRONTEND=noninteractive apt-get install -y -qq iproute2 iptables tayga >/dev/null 2>&1; echo "tayga: $(command -v tayga)"; ip tuntap add dev clat mode tun 2>&1 && echo TUN-OK; ip6tables -t nat -L POSTROUTING -n 2>&1 | head -2; ip -6 addr show scope global | grep inet; ip -6 route | head -3; python3 -c "import socket;print('ipv4only.arpa AAAA', sorted({a[4][0] for a in socket.getaddrinfo('ipv4only.arpa',0,socket.AF_INET6)}))"`));
