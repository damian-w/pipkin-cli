package pipkin

import (
	"context"
	"errors"
	"net"
)

func openCodexDesktopIPC(context.Context) (net.Conn, error) {
	return nil, errors.New("Codex task collection is unavailable on this platform")
}
