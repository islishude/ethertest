//go:build windows

package ethertest

import (
	"net"

	"github.com/Microsoft/go-winio"
)

func listenIPC(endpoint string) (net.Listener, error) {
	return winio.ListenPipe(endpoint, nil)
}
