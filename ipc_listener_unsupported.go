//go:build js || wasip1

package ethertest

import (
	"errors"
	"net"
)

func listenIPC(string) (net.Listener, error) {
	return nil, errors.New("IPC is not supported on this platform")
}
