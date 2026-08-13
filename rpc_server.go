package ethertest

import (
	"errors"
	"net"
	"net/http"
	"strings"

	"github.com/ethereum/go-ethereum/rpc"
)

func (n *Node) startServers() error {
	ethService := &ethAPI{node: n, filters: make(map[rpc.ID]*installedFilter)}
	apis := []rpc.API{
		{Namespace: "eth", Service: ethService},
		{Namespace: "net", Service: &netAPI{n}},
		{Namespace: "web3", Service: &web3API{}},
		{Namespace: "txpool", Service: &txpoolAPI{n}},
		{Namespace: "miner", Service: &minerAPI{n}},
		{Namespace: "debug", Service: &debugAPI{n}},
		{Namespace: "personal", Service: &personalAPI{n}},
		{Namespace: "ethertest", Service: &controlAPI{n}},
		{Namespace: "ethertest", Service: &walletAPI{n}},
		{Namespace: "ethertest", Service: &withdrawalAPI{n}},
		{Namespace: "ethertest", Service: &executionRequestAPI{n}},
		{Namespace: "ethertest", Service: &finalityAPI{n}},
		{Namespace: "anvil", Service: &controlAPI{n}},
		{Namespace: "evm", Service: &controlAPI{n}},
	}
	server, err := n.newRPCServer(apis)
	if err != nil {
		return err
	}
	n.rpcServer = server
	if n.cfg.IPC.Enabled {
		endpoint := n.cfg.IPCEndpoint()
		ipcServer, err := n.newRPCServer(apis)
		if err != nil {
			server.Stop()
			return err
		}
		listener, err := listenIPC(endpoint)
		if err != nil {
			ipcServer.Stop()
			server.Stop()
			n.logger.Error("IPC server failed to start",
				"event", "ipc_server_start_failed",
				"endpoint", endpoint,
				"error", err,
			)
			return err
		}
		n.ipcListener, n.ipcServer, n.ipcEndpoint = listener, ipcServer, endpoint
		n.ipcStopping.Store(false)
		go func() {
			serveErr := ipcServer.ServeListener(listener)
			if serveErr != nil && !n.ipcStopping.Load() {
				n.logger.Error("IPC server failed",
					"event", "ipc_server_failed",
					"endpoint", endpoint,
					"error", serveErr,
				)
				n.stopSignal.Do(func() { close(n.stopping) })
			}
		}()
		n.logger.Info("IPC endpoint opened", "event", "ipc_endpoint_opened", "endpoint", endpoint)
	}
	if n.cfg.HTTP.Enabled {
		listener, err := net.Listen("tcp", n.cfg.HTTP.Address)
		if err != nil {
			_ = n.stopIPC()
			server.Stop()
			return err
		}
		scheme := "http"
		if n.cfg.HTTP.TLS.CertFile != "" {
			scheme = "https"
		}
		n.httpEndpoint = scheme + "://" + listener.Addr().String()
		beaconHandler := n.beaconHandler()
		handler := corsHandler(n.cfg.HTTP.CORS, n.cfg.Limits.MaxRequestBytes, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasPrefix(r.URL.Path, "/eth/") {
				if n.cfg.Beacon.Enabled {
					beaconHandler.ServeHTTP(w, r)
				} else {
					http.NotFound(w, r)
				}
				return
			}
			if strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
				server.WebsocketHandler(n.cfg.HTTP.CORS).ServeHTTP(w, r)
				return
			}
			server.ServeHTTP(w, r)
		}))
		n.httpServer = &http.Server{Addr: n.cfg.HTTP.Address, Handler: handler, ReadHeaderTimeout: 5 * 1e9}
		go func() {
			var serveErr error
			if n.cfg.HTTP.TLS.CertFile != "" {
				serveErr = n.httpServer.ServeTLS(listener, n.cfg.HTTP.TLS.CertFile, n.cfg.HTTP.TLS.KeyFile)
			} else {
				serveErr = n.httpServer.Serve(listener)
			}
			if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
				n.logger.Error("HTTP server failed",
					"event", "http_server_failed",
					"address", n.httpEndpoint,
					"error", serveErr,
				)
				n.stopSignal.Do(func() { close(n.stopping) })
			}
		}()
	}
	return nil
}

func (n *Node) newRPCServer(apis []rpc.API) (*rpc.Server, error) {
	server := rpc.NewServer()
	server.SetBatchLimits(n.cfg.Limits.MaxBatchItems, int(n.cfg.Limits.MaxResponseBytes))
	for _, api := range apis {
		if err := server.RegisterName(api.Namespace, api.Service); err != nil {
			server.Stop()
			return nil, err
		}
	}
	return server, nil
}

func (n *Node) stopIPC() error {
	if n.ipcListener == nil && n.ipcServer == nil {
		return nil
	}
	endpoint := n.ipcEndpoint
	n.ipcStopping.Store(true)
	var err error
	if n.ipcListener != nil {
		err = n.ipcListener.Close()
		if errors.Is(err, net.ErrClosed) {
			err = nil
		}
		n.ipcListener = nil
	}
	if n.ipcServer != nil {
		n.ipcServer.Stop()
		n.ipcServer = nil
	}
	n.logger.Info("IPC endpoint closed", "event", "ipc_endpoint_closed", "endpoint", endpoint)
	return err
}

func corsHandler(origins []string, maxBytes int64, next http.Handler) http.Handler {
	allowAll := false
	allowed := make(map[string]struct{}, len(origins))
	for _, origin := range origins {
		allowed[origin] = struct{}{}
		allowAll = allowAll || origin == "*"
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if allowAll {
			w.Header().Set("Access-Control-Allow-Origin", "*")
		} else if _, ok := allowed[origin]; ok {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Add("Vary", "Origin")
		}
		w.Header().Set("Access-Control-Allow-Headers", "content-type")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.Body != nil {
			r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
		}
		next.ServeHTTP(w, r)
	})
}
