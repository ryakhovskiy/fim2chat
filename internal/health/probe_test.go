package health

import (
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestProbe(t *testing.T) {
	status := http.StatusOK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/health" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		w.WriteHeader(status)
	}))
	defer srv.Close()
	addr := srv.Listener.Addr().String()
	_, port, _ := net.SplitHostPort(addr)

	for _, listen := range []string{addr, "0.0.0.0:" + port, ":" + port} {
		if err := Probe(listen); err != nil {
			t.Errorf("Probe(%q): %v", listen, err)
		}
	}
	status = http.StatusServiceUnavailable
	if Probe(addr) == nil {
		t.Error("a 503 must be unhealthy")
	}
	if Probe("not-an-address") == nil {
		t.Error("a malformed address must be unhealthy")
	}
	srv.Close()
	if Probe(addr) == nil {
		t.Error("a dead server must be unhealthy")
	}
}
