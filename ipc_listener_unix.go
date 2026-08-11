//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package ethertest

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"syscall"
)

func listenIPC(endpoint string) (net.Listener, error) {
	maxPathSize := len(syscall.RawSockaddrUnix{}.Path)
	if len(endpoint)+1 > maxPathSize {
		return nil, fmt.Errorf("IPC endpoint is longer than %d characters", maxPathSize-1)
	}
	if err := os.MkdirAll(filepath.Dir(endpoint), 0o751); err != nil {
		return nil, err
	}
	if err := os.Remove(endpoint); err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	listener, err := net.Listen("unix", endpoint)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(endpoint, 0o600); err != nil {
		_ = listener.Close()
		return nil, err
	}
	return listener, nil
}
