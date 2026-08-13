package main

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"gpb/internal/config"
	"gpb/internal/web"
)

const healthyState = "ok"

// fetchState asks the running daemon rather than opening the browser profile itself: the
// daemon holds that profile, and two Chromium instances on one user data directory clash.
func fetchState(cfg config.Config) (string, error) {
	transport, err := transportFor(cfg)
	if err != nil {
		return "", err
	}
	client := &http.Client{Timeout: 10 * time.Second, Transport: transport}

	response, err := client.Get(healthURL(cfg))
	if err != nil {
		return "", fmt.Errorf("the daemon is not answering: %w", err)
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("daemon returned %s", response.Status)
	}

	body, err := io.ReadAll(io.LimitReader(response.Body, 1024))
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(body)), nil
}

// transportFor trusts the daemon's own certificate and nothing else, which is a stronger check
// than a public trust store rather than a weaker one: what answers has to hold the key to the
// certificate this machine's config names. The name is taken from that certificate too, because
// the address dialled is 127.0.0.1 and a provided certificate is issued for the name people
// browse to — refusing it for that would report a healthy daemon as a dead one every time the
// container's health command ran.
func transportFor(cfg config.Config) (http.RoundTripper, error) {
	if !cfg.Web.TLS.Enabled() {
		return nil, nil
	}

	certificate, err := web.CertificateServed(cfg)
	if err != nil {
		return nil, fmt.Errorf("reading the certificate the daemon serves with: %w", err)
	}
	pinned := x509.NewCertPool()
	pinned.AddCert(certificate)

	return &http.Transport{TLSClientConfig: &tls.Config{
		RootCAs:    pinned,
		ServerName: nameOn(certificate),
	}}, nil
}

func nameOn(certificate *x509.Certificate) string {
	if len(certificate.DNSNames) > 0 {
		return certificate.DNSNames[0]
	}
	if len(certificate.IPAddresses) > 0 {
		return certificate.IPAddresses[0].String()
	}
	return certificate.Subject.CommonName
}

func healthURL(cfg config.Config) string {
	host, port, err := net.SplitHostPort(cfg.Web.Listen)
	if err != nil {
		host, port = "127.0.0.1", "8080"
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	return cfg.Web.TLS.Scheme() + "://" + net.JoinHostPort(host, port) + "/healthz"
}
