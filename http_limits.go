package ethertest

import (
	"bytes"
	"fmt"
	"net/http"
	"strings"
)

type boundedHTTPResponse struct {
	header   http.Header
	status   int
	buffer   bytes.Buffer
	limit    int64
	overflow bool
}

func newBoundedHTTPResponse(limit int64) *boundedHTTPResponse {
	return &boundedHTTPResponse{header: make(http.Header), limit: limit}
}

func (response *boundedHTTPResponse) Header() http.Header { return response.header }

func (response *boundedHTTPResponse) WriteHeader(status int) {
	if response.status == 0 {
		response.status = status
	}
}

func (response *boundedHTTPResponse) Write(data []byte) (int, error) {
	if response.status == 0 {
		response.status = http.StatusOK
	}
	if response.overflow || int64(response.buffer.Len())+int64(len(data)) > response.limit {
		response.overflow = true
		return 0, fmt.Errorf("HTTP response exceeds %d bytes", response.limit)
	}
	return response.buffer.Write(data)
}

func responseLimitHandler(limit int64, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.EqualFold(r.Header.Get("Upgrade"), "websocket") || r.URL.Path == "/eth/v1/events" {
			next.ServeHTTP(w, r)
			return
		}
		response := newBoundedHTTPResponse(limit)
		next.ServeHTTP(response, r)
		if response.overflow {
			http.Error(w, "response resource limit exceeded", http.StatusRequestEntityTooLarge)
			return
		}
		for key, values := range response.header {
			w.Header()[key] = append([]string(nil), values...)
		}
		status := response.status
		if status == 0 {
			status = http.StatusOK
		}
		w.WriteHeader(status)
		_, _ = w.Write(response.buffer.Bytes())
	})
}
