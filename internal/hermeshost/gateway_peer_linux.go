//go:build linux

package hermeshost

import (
	"fmt"
	"net"
	"os"
	"syscall"
)

// gatewayPeerUID is the uid the gateway must run as: this process's (a test
// changes it to reach the refusal, which needs no second user).
var gatewayPeerUID = os.Getuid

// peerIsUs requires the process at the other end of the unix connection c to
// run as this uid (SO_PEERCRED).
func peerIsUs(c net.Conn) error {
	uc, ok := c.(*net.UnixConn)
	if !ok {
		return fmt.Errorf("%w: not a unix socket", errGatewayUnsafe)
	}
	raw, err := uc.SyscallConn()
	if err != nil {
		return err
	}
	var cred *syscall.Ucred
	var cerr error
	if err := raw.Control(func(fd uintptr) {
		cred, cerr = syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
	}); err != nil {
		return err
	}
	if cerr != nil {
		return fmt.Errorf("%w: cannot read the gateway's credentials: %v", errGatewayUnsafe, cerr)
	}
	if want := gatewayPeerUID(); int(cred.Uid) != want {
		return fmt.Errorf("%w: the process on its socket runs as uid %d, not this user's %d", errGatewayUnsafe, cred.Uid, want)
	}
	return nil
}
