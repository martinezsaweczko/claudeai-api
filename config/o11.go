package config

import "fmt"

type O11Config struct {
	TracerEndpoint string
	PrometheusPath string
	Environment    string
}

// Validate checks if the o11 configuration is valid.
func (c *O11Config) Validate() error {
	// Add validation logic for the o11 configuration fields if necessary.
	// Check trace endpoints doesn't come with http or https, as they should be gRPC endpoints.
	if c.TracerEndpoint != "" && (startsWithHTTP(c.TracerEndpoint) || startsWithHTTPS(c.TracerEndpoint)) {
		return fmt.Errorf("tracer endpoint should not start with http:// or https://")
	}
	//Prometheus path should be a valid path, starting with /
	if c.PrometheusPath != "" && c.PrometheusPath[0] != '/' {
		return fmt.Errorf("prometheus path should start with /")
	}
	return nil
}

func startsWithHTTP(s string) bool {
	return len(s) >= 7 && s[:7] == "http://"
}

func startsWithHTTPS(s string) bool {
	return len(s) >= 8 && s[:8] == "https://"
}
