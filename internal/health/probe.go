// Package health implements the client side of the /health endpoint, used by
// the Docker HEALTHCHECK (the image has no curl or shell).
package health

import (
	"fmt"
	"net"
	"net/http"
	"time"
)

// Probe calls /health on the address a server listens on and returns nil if it
// answers 200.
func Probe(listen string) error {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return err
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1" // a wildcard listen address is not dialable
	}
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get("http://" + net.JoinHostPort(host, port) + "/health")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	return nil
}
