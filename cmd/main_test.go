package main

import (
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestProbeHealth(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/health":
			// Health is public: no Authorization header is ever sent or needed.
			if r.Header.Get("Authorization") != "" {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	}))
	defer srv.Close()

	if got := probeHealth(srv.URL + "/api/v1/health"); got != 0 {
		t.Errorf("healthy server: exit %d, want 0", got)
	}
	if got := probeHealth(srv.URL + "/other"); got != 1 {
		t.Errorf("non-200 answer: exit %d, want 1", got)
	}

	// Nothing listening: closed port.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	if got := probeHealth("http://" + addr + "/api/v1/health"); got != 1 {
		t.Errorf("closed port: exit %d, want 1", got)
	}
}

func TestHealthCheckDefaultsToSinglePort(t *testing.T) {
	if defaultHealthPort != "46564" {
		t.Errorf("default health-check port = %s, want 46564 (the single server)", defaultHealthPort)
	}
}

// The probe dials loopback, which must reach a server bound to every interface.
func TestProbeHealthReachesWildcardBind(t *testing.T) {
	l, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		t.Skipf("cannot bind 0.0.0.0: %v", err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})}
	go func() { _ = srv.Serve(l) }()
	defer srv.Close()

	_, port, _ := net.SplitHostPort(l.Addr().String())
	if got := probeHealth("http://127.0.0.1:" + port + "/api/v1/health"); got != 0 {
		t.Errorf("wildcard-bound server: exit %d, want 0", got)
	}
}
