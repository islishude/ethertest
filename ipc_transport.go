package ethertest

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"unicode"

	"github.com/ethereum/go-ethereum/rpc"
)

type boundedIPCConn struct {
	net.Conn
	reader      *bufio.Reader
	pending     []byte
	requestMax  int
	responseMax int
}

func newBoundedIPCConn(connection net.Conn, requestMax, responseMax int) *boundedIPCConn {
	return &boundedIPCConn{
		Conn: connection, reader: bufio.NewReaderSize(connection, min(requestMax+1, 32<<10)),
		requestMax: requestMax, responseMax: responseMax,
	}
}

func (connection *boundedIPCConn) Read(output []byte) (int, error) {
	if len(connection.pending) == 0 {
		value, err := connection.readJSONValue()
		if err != nil {
			return 0, err
		}
		connection.pending = value
	}
	n := copy(output, connection.pending)
	connection.pending = connection.pending[n:]
	return n, nil
}

// readJSONValue frames one top-level JSON-RPC object or batch independently of
// write boundaries and optional newlines. This lets a persistent JSON decoder
// consume adjacent values while enforcing the byte limit on each value.
func (connection *boundedIPCConn) readJSONValue() ([]byte, error) {
	value := make([]byte, 0, min(connection.requestMax, 4<<10))
	skipped := 0
	depth := 0
	inString, escaped := false, false
	for {
		current, err := connection.reader.ReadByte()
		if err != nil {
			if errors.Is(err, io.EOF) && len(value) != 0 {
				return value, nil
			}
			return nil, err
		}
		if depth == 0 && len(value) == 0 {
			if unicode.IsSpace(rune(current)) {
				skipped++
				if skipped > connection.requestMax {
					return nil, fmt.Errorf("IPC request exceeds %d bytes", connection.requestMax)
				}
				continue
			}
			if current != '{' && current != '[' {
				return []byte{current}, nil
			}
			depth = 1
			value = append(value, current)
			continue
		}
		value = append(value, current)
		if len(value) > connection.requestMax {
			return nil, fmt.Errorf("IPC request exceeds %d bytes", connection.requestMax)
		}
		if inString {
			switch {
			case escaped:
				escaped = false
			case current == '\\':
				escaped = true
			case current == '"':
				inString = false
			}
			continue
		}
		switch current {
		case '"':
			inString = true
		case '{', '[':
			depth++
		case '}', ']':
			depth--
			if depth == 0 {
				return value, nil
			}
		}
	}
}

func (connection *boundedIPCConn) Write(input []byte) (int, error) {
	if len(input) > connection.responseMax {
		_ = connection.Close()
		return 0, fmt.Errorf("IPC response exceeds %d bytes", connection.responseMax)
	}
	written := 0
	for written < len(input) {
		n, err := connection.Conn.Write(input[written:])
		written += n
		if err != nil {
			return written, err
		}
		if n == 0 {
			return written, io.ErrShortWrite
		}
	}
	return written, nil
}

func (n *Node) serveIPC(server *rpc.Server, listener net.Listener) error {
	for {
		connection, err := listener.Accept()
		if err != nil {
			if temporary, ok := err.(interface{ Temporary() bool }); ok && temporary.Temporary() {
				continue
			}
			return err
		}
		bounded := newBoundedIPCConn(
			connection, int(n.cfg.Limits.MaxRequestBytes), int(n.cfg.Limits.MaxResponseBytes),
		)
		n.ipcHandlers.Go(func() {
			server.ServeCodec(rpc.NewCodec(bounded), 0)
		})
	}
}
