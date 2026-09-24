// Kernel feature probe for running openshell-sandbox inside a Blaxel microVM.
// Checks: Landlock ABI, seccomp user notification (end to end), network
// namespaces (root and unprivileged), and Unix sockets across netns.
package main

import (
	"encoding/binary"
	"fmt"
	"net"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

func report(name string, ok bool, detail string) {
	status := "FAIL"
	if ok {
		status = "OK"
	}
	fmt.Printf("%-28s %-4s %s\n", name, status, detail)
}

func landlock() {
	const createRulesetVersion = 1
	v, _, errno := unix.Syscall(unix.SYS_LANDLOCK_CREATE_RULESET, 0, 0, createRulesetVersion)
	if errno != 0 {
		report("landlock", false, errno.Error())
		return
	}
	report("landlock", true, fmt.Sprintf("ABI v%d", v))
}

// seccomp user notification: install a filter on one locked OS thread that
// routes getppid to the listener, answer it from another thread, and check
// the injected value comes back.
func seccompNotify() {
	done := make(chan string, 1)
	fdCh := make(chan int, 1)
	go func() {
		runtime.LockOSThread() // never unlocked: thread dies with the filter
		if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
			fdCh <- -1
			done <- "no_new_privs: " + err.Error()
			return
		}
		filter := []unix.SockFilter{
			{Code: unix.BPF_LD | unix.BPF_W | unix.BPF_ABS, K: 0}, // seccomp_data.nr
			{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, Jt: 0, Jf: 1, K: unix.SYS_GETPPID},
			{Code: unix.BPF_RET | unix.BPF_K, K: unix.SECCOMP_RET_USER_NOTIF},
			{Code: unix.BPF_RET | unix.BPF_K, K: unix.SECCOMP_RET_ALLOW},
		}
		prog := unix.SockFprog{Len: uint16(len(filter)), Filter: &filter[0]}
		fd, _, errno := unix.Syscall(unix.SYS_SECCOMP, unix.SECCOMP_SET_MODE_FILTER,
			unix.SECCOMP_FILTER_FLAG_NEW_LISTENER, uintptr(unsafe.Pointer(&prog)))
		if errno != 0 {
			fdCh <- -1
			done <- "seccomp NEW_LISTENER: " + errno.Error()
			return
		}
		fdCh <- int(fd)
		r, _, _ := unix.Syscall(unix.SYS_GETPPID, 0, 0, 0)
		done <- fmt.Sprintf("getppid returned %d", r)
	}()

	fd := <-fdCh
	if fd < 0 {
		report("seccomp user-notif", false, <-done)
		return
	}
	go func() {
		// struct seccomp_notif: id u64, pid u32, flags u32, data (64 bytes)
		req := make([]byte, 80)
		if _, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), 0xc0502100, // SECCOMP_IOCTL_NOTIF_RECV
			uintptr(unsafe.Pointer(&req[0]))); errno != 0 {
			done <- "NOTIF_RECV: " + errno.Error()
			return
		}
		// struct seccomp_notif_resp: id u64, val i64, error i32, flags u32
		resp := make([]byte, 24)
		copy(resp[0:8], req[0:8])
		binary.LittleEndian.PutUint64(resp[8:16], 4242)
		if _, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), 0xc0182101, // SECCOMP_IOCTL_NOTIF_SEND
			uintptr(unsafe.Pointer(&resp[0]))); errno != 0 {
			done <- "NOTIF_SEND: " + errno.Error()
		}
	}()
	select {
	case msg := <-done:
		report("seccomp user-notif", strings.HasSuffix(msg, " 4242"), msg)
	case <-time.After(5 * time.Second):
		report("seccomp user-notif", false, "timeout")
	}
}

func netnsChild(flags uintptr, name string) {
	cmd := exec.Command("/proc/self/exe", "netns-child")
	cmd.SysProcAttr = &syscall.SysProcAttr{Cloneflags: flags}
	if flags&unix.CLONE_NEWUSER != 0 {
		cmd.SysProcAttr.UidMappings = []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getuid(), Size: 1}}
		cmd.SysProcAttr.GidMappings = []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getgid(), Size: 1}}
	}
	cmd.Env = append(os.Environ(), "PROBE_SOCK=/tmp/probe.sock")
	out, err := cmd.CombinedOutput()
	if err != nil {
		report(name, false, fmt.Sprintf("%v: %s", err, strings.TrimSpace(string(out))))
		return
	}
	report(name, true, strings.TrimSpace(string(out)))
}

// Runs inside a fresh netns: list interfaces, try outbound TCP, and connect to
// a Unix socket that lives in the parent netns.
func netnsChildMain() {
	ifs, _ := net.Interfaces()
	var names []string
	for _, i := range ifs {
		names = append(names, i.Name)
	}
	_, dialErr := net.DialTimeout("tcp", "1.1.1.1:443", 2*time.Second)
	egress := "egress blocked"
	if dialErr == nil {
		egress = "EGRESS OPEN"
	}
	c, err := net.Dial("unix", os.Getenv("PROBE_SOCK"))
	uds := "uds ok"
	if err != nil {
		uds = "uds FAILED: " + err.Error()
	} else {
		buf := make([]byte, 4)
		c.Read(buf)
		uds = "uds read " + string(buf)
		c.Close()
	}
	fmt.Printf("ifaces=%v, %s, %s", names, egress, uds)
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "netns-child" {
		netnsChildMain()
		return
	}
	var u unix.Utsname
	unix.Uname(&u)
	fmt.Printf("kernel: %s %s uid=%d\n", unix.ByteSliceToString(u.Release[:]),
		unix.ByteSliceToString(u.Machine[:]), os.Getuid())
	if b, err := os.ReadFile("/sys/kernel/security/lsm"); err == nil {
		fmt.Printf("lsm: %s\n", strings.TrimSpace(string(b)))
	}
	for _, f := range []string{"/proc/sys/user/max_user_namespaces", "/proc/sys/kernel/unprivileged_userns_clone"} {
		if b, err := os.ReadFile(f); err == nil {
			fmt.Printf("%s: %s\n", f, strings.TrimSpace(string(b)))
		}
	}

	os.Remove("/tmp/probe.sock")
	l, err := net.Listen("unix", "/tmp/probe.sock")
	if err == nil {
		os.Chmod("/tmp/probe.sock", 0o777)
		go func() {
			for {
				c, err := l.Accept()
				if err != nil {
					return
				}
				c.Write([]byte("pong"))
				c.Close()
			}
		}()
	}

	landlock()
	seccompNotify()
	netnsChild(unix.CLONE_NEWNET, "netns (root)")
	netnsChild(unix.CLONE_NEWUSER|unix.CLONE_NEWNET, "netns (unpriv userns)")
}
