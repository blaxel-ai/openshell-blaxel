import { SandboxInstance } from "@blaxel/core";
const sb = await SandboxInstance.get("os-exp-gw");
const sh = async (command, t = 60) => (await sb.process.exec({ command, waitForCompletion: true, timeout: t })).logs;
console.log(await sh("mkdir -p /srv/os && cd /srv/os && [ -d tls ] || openshell-gateway generate-certs --output-dir /srv/os/tls --server-san host.openshell.internal 2>&1 | tail -2; find /srv/os/tls -type f | sort"));
await sb.process.exec({ name: "podman", command: "podman system service --time=0 unix:///run/podman/podman.sock", keepAlive: false });
await sh("for i in $(seq 50); do [ -S /run/podman/podman.sock ] && break; sleep 0.2; done; ls -la /run/podman/");
await sb.process.exec({
  name: "gateway",
  command: "OPENSHELL_LOCAL_TLS_DIR=/srv/os/tls OPENSHELL_DRIVERS=podman openshell-gateway --name blaxel-exp --bind-address 127.0.0.1 --port 17670 --db-url 'sqlite:/srv/os/gateway.db?mode=rwc' --log-level info",
  keepAlive: true,
});
await sb.process.exec({ name: "relay", command: "wsrelay serve -listen 0.0.0.0:9000 -target tcp:127.0.0.1:17670", waitForPorts: [9000] });
await new Promise((r) => setTimeout(r, 4000));
const g = await sb.process.get("gateway");
console.log("gateway status:", g.status);
console.log(await sh("ss -ltnp 2>/dev/null | grep -E '17670|9000' || netstat -ltn"));
const logs = await sb.process.logs("gateway", "all");
console.log(String(logs).split("\n").slice(-12).join("\n"));
