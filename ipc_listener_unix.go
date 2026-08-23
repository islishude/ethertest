//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package ethertest

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

func listenIPC(endpoint string) (net.Listener, error) {
	maxPathSize := len(syscall.RawSockaddrUnix{}.Path)
	if len(endpoint)+1 > maxPathSize {
		return nil, fmt.Errorf("IPC endpoint is longer than %d characters", maxPathSize-1)
	}
	if err := os.MkdirAll(filepath.Dir(endpoint), 0o751); err != nil {
		return nil, err
	}
	if info, err := os.Lstat(endpoint); err == nil {
		if info.Mode()&os.ModeSocket == 0 {
			return nil, fmt.Errorf("IPC endpoint exists and is not a Unix socket: %s", endpoint)
		}
		connection, dialErr := net.DialTimeout("unix", endpoint, 100*time.Millisecond)
		if dialErr == nil {
			_ = connection.Close()
			return nil, fmt.Errorf("IPC endpoint is already in use: %s", endpoint)
		}
		if !errors.Is(dialErr, syscall.ECONNREFUSED) && !os.IsNotExist(dialErr) {
			return nil, fmt.Errorf("check existing IPC endpoint %s: %w", endpoint, dialErr)
		}
		if err := os.Remove(endpoint); err != nil {
			return nil, err
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	listener, err := net.Listen("unix", endpoint)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(endpoint, 0o600); err != nil {
		_ = listener.Close()
		_ = os.Remove(endpoint)
		return nil, err
	}
	return listener, nil
}
