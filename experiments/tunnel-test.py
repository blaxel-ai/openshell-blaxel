import socket, time, os, sys, threading
port = int(sys.argv[1])
def conn():
    s = socket.create_connection(("127.0.0.1", port), timeout=30); s.setsockopt(socket.IPPROTO_TCP, socket.TCP_NODELAY, 1); return s
def rt(s, data):
    s.sendall(data); got = b""
    while len(got) < len(data): got += s.recv(1 << 20)
    return got == data
s = conn()
lat = []
for _ in range(20):
    t = time.time(); assert rt(s, b"ping"); lat.append((time.time() - t) * 1000)
lat.sort(); print(f"rtt ms: p50={lat[10]:.1f} max={lat[-1]:.1f}")
blob = os.urandom(8 << 20); t = time.time()
def reader(out):
    got = 0
    while got < len(blob): got += len(s.recv(1 << 20))
    out.append(got)
out = []; th = threading.Thread(target=reader, args=(out,)); th.start(); s.sendall(blob); th.join()
dt = time.time() - t; print(f"8MiB echo: {dt:.2f}s ({2*8/dt:.1f} MiB/s both ways)")
res = []
def worker(i):
    c = conn(); res.append(rt(c, f"conn-{i}".encode() * 1000)); c.close()
ths = [threading.Thread(target=worker, args=(i,)) for i in range(8)]; [x.start() for x in ths]; [x.join() for x in ths]
print("8 concurrent conns ok:", all(res), len(res))
idle = int(sys.argv[2]) if len(sys.argv) > 2 else 0
if idle:
    time.sleep(idle)
    try: print(f"after {idle}s idle: roundtrip ok =", rt(s, b"still-there"))
    except Exception as e: print(f"after {idle}s idle: FAILED", e)
