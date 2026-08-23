package ethertest

import (
	"errors"
	"io"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/rpc"
	"github.com/gorilla/websocket"
)

type boundedWebsocketConn struct {
	connection  *websocket.Conn
	reader      io.Reader
	responseMax int
	writeMu     sync.Mutex
	closed      chan struct{}
	closeOnce   sync.Once
}

func newBoundedWebsocketConn(connection *websocket.Conn, requestMax int64, responseMax int) *boundedWebsocketConn {
	connection.SetReadLimit(requestMax)
	return &boundedWebsocketConn{connection: connection, responseMax: responseMax, closed: make(chan struct{})}
}

func (connection *boundedWebsocketConn) Read(output []byte) (int, error) {
	for {
		if connection.reader == nil {
			messageType, reader, err := connection.connection.NextReader()
			if err != nil {
				return 0, err
			}
			if messageType != websocket.TextMessage && messageType != websocket.BinaryMessage {
				continue
			}
			connection.reader = reader
		}
		n, err := connection.reader.Read(output)
		if errors.Is(err, io.EOF) {
			connection.reader = nil
			if n != 0 {
				return n, nil
			}
			continue
		}
		return n, err
	}
}

func (connection *boundedWebsocketConn) Write(input []byte) (int, error) {
	if len(input) > connection.responseMax {
		_ = connection.Close()
		return 0, newResourceLimitError("WebSocket response bytes", uint64(len(input)), uint64(connection.responseMax))
	}
	connection.writeMu.Lock()
	defer connection.writeMu.Unlock()
	if err := connection.connection.WriteMessage(websocket.TextMessage, input); err != nil {
		return 0, err
	}
	return len(input), nil
}

func (connection *boundedWebsocketConn) Close() error {
	var err error
	connection.closeOnce.Do(func() {
		close(connection.closed)
		connection.writeMu.Lock()
		_ = connection.connection.WriteControl(
			websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), time.Now().Add(time.Second),
		)
		connection.writeMu.Unlock()
		err = connection.connection.Close()
	})
	return err
}

func (connection *boundedWebsocketConn) SetWriteDeadline(deadline time.Time) error {
	return connection.connection.SetWriteDeadline(deadline)
}

func (connection *boundedWebsocketConn) RemoteAddr() string {
	return connection.connection.RemoteAddr().String()
}

func (connection *boundedWebsocketConn) pingLoop() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-connection.closed:
			return
		case <-ticker.C:
			connection.writeMu.Lock()
			err := connection.connection.WriteControl(websocket.PingMessage, nil, time.Now().Add(5*time.Second))
			connection.writeMu.Unlock()
			if err != nil {
				_ = connection.Close()
				return
			}
		}
	}
}

func (n *Node) websocketHandler(server *rpc.Server) http.Handler {
	upgrader := websocket.Upgrader{
		ReadBufferSize: 1024, WriteBufferSize: 1024,
		CheckOrigin: func(request *http.Request) bool {
			origin := request.Header.Get("Origin")
			return origin == "" || slices.Contains(n.cfg.HTTP.CORS, "*") || slices.Contains(n.cfg.HTTP.CORS, origin)
		},
	}
	return http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		connection, err := upgrader.Upgrade(w, request, nil)
		if err != nil {
			return
		}
		bounded := newBoundedWebsocketConn(
			connection, n.cfg.Limits.MaxRequestBytes, int(n.cfg.Limits.MaxResponseBytes),
		)
		go bounded.pingLoop()
		server.ServeCodec(rpc.NewCodec(bounded), 0)
	})
}
