//go:build !linux

package hermeshost

import (
	"fmt"
	"net"
)

// peerIsUs refuses: this platform has no SO_PEERCRED here, so the process on
// the gateway's socket cannot be authenticated, and muster attaches no viewer
// (every Hermes ask is then answered in the terminal). A false refusal is the
// safe way round; Hermes's own client falls back to the path checks alone.
func peerIsUs(net.Conn) error {
	return fmt.Errorf("%w: this platform cannot authenticate the process on its socket", errGatewayUnsafe)
}
