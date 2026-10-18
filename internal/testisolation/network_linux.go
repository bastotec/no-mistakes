//go:build linux

package testisolation

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"net"
	"os"
	"runtime"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

// RestrictWorkerNetwork irreversibly restricts every worker thread and every
// descendant to the database/model TCP ports. A new network namespace alone
// is not a port allowlist. Landlock needs ABI 4+ and a non-cgo worker so Go can
// apply the rule to ALL threads, including threads created after this call.
func RestrictWorkerNetwork() error {
	abi, _, errno := unix.Syscall(unix.SYS_LANDLOCK_CREATE_RULESET, 0, 0, unix.LANDLOCK_CREATE_RULESET_VERSION)
	if errno != 0 || abi < 4 {
		return fmt.Errorf("test-isolation-network-policy: Landlock network ABI 4+ is required")
	}
	attr := unix.LandlockRulesetAttr{Access_net: unix.LANDLOCK_ACCESS_NET_BIND_TCP | unix.LANDLOCK_ACCESS_NET_CONNECT_TCP}
	fd, _, errno := unix.Syscall(unix.SYS_LANDLOCK_CREATE_RULESET, uintptr(unsafe.Pointer(&attr)), 16, 0)
	if errno != 0 {
		return fmt.Errorf("test-isolation-network-policy: create ruleset: %w", errno)
	}
	defer unix.Close(int(fd))
	for _, port := range []uint64{17681, 17682} {
		rule := struct{ Access, Port uint64 }{attr.Access_net, port}
		_, _, errno = unix.Syscall6(unix.SYS_LANDLOCK_ADD_RULE, fd, 2, uintptr(unsafe.Pointer(&rule)), 0, 0, 0)
		if errno != 0 {
			return fmt.Errorf("test-isolation-network-policy: add port rule: %w", errno)
		}
	}
	_, _, errno = syscall.AllThreadsSyscall(unix.SYS_PRCTL, unix.PR_SET_NO_NEW_PRIVS, 1, 0)
	if errno != 0 {
		return fmt.Errorf("test-isolation-network-policy: all-thread no-new-privileges unavailable (build with CGO_ENABLED=0): %w", errno)
	}
	_, _, errno = syscall.AllThreadsSyscall(unix.SYS_LANDLOCK_RESTRICT_SELF, fd, 0, 0)
	if errno != 0 {
		return fmt.Errorf("test-isolation-network-policy: all-thread confinement unavailable: %w", errno)
	}
	return nil
}

// socketFilter closes the channels Landlock does not mediate: UDP, raw/other
// socket families, Unix path sockets, and MPTCP (which is not TCP for Landlock).
// Only ordinary TCP and private Unix STREAM socketpairs remain creatable. The
// sole host-model Unix stream is already connected before this filter loads.
func socketFilter() ([]byte, error) {
	var arch uint32
	switch runtime.GOARCH {
	case "amd64":
		arch = unix.AUDIT_ARCH_X86_64
	case "arm64":
		arch = unix.AUDIT_ARCH_AARCH64
	default:
		return nil, fmt.Errorf("test-isolation-platform: socket policy supports Linux amd64/arm64 only")
	}
	// Labels: inspectPair=17, deny=22, allow=23. Check architecture and x32
	// before dispatch so alternate syscall ABIs cannot bypass socket policy.
	// io_uring could otherwise create sockets without a socket(2) syscall.
	filters := []unix.SockFilter{
		{Code: unix.BPF_LD | unix.BPF_W | unix.BPF_ABS, K: 4},
		{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, K: arch, Jt: 1},
		{Code: unix.BPF_RET | unix.BPF_K, K: unix.SECCOMP_RET_KILL_PROCESS},
		{Code: unix.BPF_LD | unix.BPF_W | unix.BPF_ABS, K: 0},
		{Code: unix.BPF_JMP | unix.BPF_JSET | unix.BPF_K, K: 0x40000000, Jt: 17},
		{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, K: unix.SYS_IO_URING_SETUP, Jt: 16},
		{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, K: unix.SYS_SOCKET, Jt: 1},
		{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, K: unix.SYS_SOCKETPAIR, Jt: 9, Jf: 15},
		{Code: unix.BPF_LD | unix.BPF_W | unix.BPF_ABS, K: 16},
		{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, K: unix.AF_INET, Jt: 1},
		{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, K: unix.AF_INET6, Jf: 11},
		{Code: unix.BPF_LD | unix.BPF_W | unix.BPF_ABS, K: 24},
		{Code: unix.BPF_ALU | unix.BPF_AND | unix.BPF_K, K: 0xf},
		{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, K: unix.SOCK_STREAM, Jf: 8},
		{Code: unix.BPF_LD | unix.BPF_W | unix.BPF_ABS, K: 32},
		{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, K: 0, Jt: 7},
		{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, K: unix.IPPROTO_TCP, Jt: 6, Jf: 5},
		// socketpair: AF_UNIX + SOCK_STREAM only. It cannot be reconnected to
		// host path sockets, unlike a Unix datagram socketpair.
		{Code: unix.BPF_LD | unix.BPF_W | unix.BPF_ABS, K: 16},
		{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, K: unix.AF_UNIX, Jf: 3},
		{Code: unix.BPF_LD | unix.BPF_W | unix.BPF_ABS, K: 24},
		{Code: unix.BPF_ALU | unix.BPF_AND | unix.BPF_K, K: 0xf},
		{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, K: unix.SOCK_STREAM, Jt: 1},
		{Code: unix.BPF_RET | unix.BPF_K, K: unix.SECCOMP_RET_ERRNO | uint32(syscall.EPERM)},
		{Code: unix.BPF_RET | unix.BPF_K, K: unix.SECCOMP_RET_ALLOW},
	}
	var encoded bytes.Buffer
	if err := binary.Write(&encoded, binary.LittleEndian, filters); err != nil {
		return nil, err
	}
	return encoded.Bytes(), nil
}

func modelChannel() (net.Conn, *os.File, error) {
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return nil, nil, err
	}
	host := os.NewFile(uintptr(fds[0]), "model-host")
	guest := os.NewFile(uintptr(fds[1]), "model-worker")
	connection, err := net.FileConn(host)
	host.Close()
	if err != nil {
		guest.Close()
		return nil, nil, err
	}
	return connection, guest, nil
}
