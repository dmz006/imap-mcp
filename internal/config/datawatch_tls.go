package config

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net/http"
	"os"
)

// Transport returns the HTTP transport for every call imap-mcp makes to
// datawatch (secrets, capacity gate, LLM proxy). With ca_file set, that PEM
// certificate is trusted in addition to the system roots (AGENT.md D15a);
// verification is never disabled. An unreadable file, or one holding no
// certificate, is an error so startup fails closed.
func (d *DatawatchConfig) Transport() (http.RoundTripper, error) {
	if d == nil {
		return http.DefaultTransport, nil
	}
	if d.transport != nil {
		return d.transport, nil
	}
	t := http.DefaultTransport.(*http.Transport).Clone()
	if d.CAFile != "" {
		pem, err := os.ReadFile(d.CAFile)
		if err != nil {
			return nil, fmt.Errorf("datawatch.ca_file: %w", err)
		}
		pool, err := x509.SystemCertPool()
		if err != nil || pool == nil {
			pool = x509.NewCertPool()
		}
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("datawatch.ca_file %s: no PEM certificate found", d.CAFile)
		}
		t.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	}
	d.transport = t
	return t, nil
}
