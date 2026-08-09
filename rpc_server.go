package ethertest

import (
	"errors"
	"net"
	"net/http"
	"strings"

	"github.com/ethereum/go-ethereum/rpc"
)

func (n *Node) startServers() error {
	server := rpc.NewServer()
	server.SetBatchLimits(n.cfg.Limits.MaxBatchItems, int(n.cfg.Limits.MaxResponseBytes))
	ethService := &ethAPI{node: n, filters: make(map[rpc.ID]*installedFilter)}
	services := []struct {
		namespace string
		service   any
	}{
		{"eth", ethService},
		{"net", &netAPI{n}},
		{"web3", &web3API{}},
		{"txpool", &txpoolAPI{n}},
		{"miner", &minerAPI{n}},
		{"debug", &debugAPI{n}},
		{"personal", &personalAPI{n}},
		{"ethertest", &controlAPI{n}},
		{"anvil", &controlAPI{n}},
		{"evm", &controlAPI{n}},
	}
	for _, service := range services {
		if err := server.RegisterName(service.namespace, service.service); err != nil {
			server.Stop()
			return err
		}
	}
	n.rpcServer = server
	if n.cfg.HTTP.Enabled {
		listener, err := net.Listen("tcp", n.cfg.HTTP.Address)
		if err != nil {
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
