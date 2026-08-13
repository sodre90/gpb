package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"gpb/internal/config"
)

// The health command is the container's HealthCmd. Dialling http at a daemon that has been moved
// to https answered 400 and reported a perfectly well daemon as a dead one, which is how it was
// found: on the box, minutes after encryption was turned on there.
func TestTheHealthCheckFollowsTheEncryptionSetting(t *testing.T) {
	cfg := config.Defaults()
	cfg.Web.Listen = ":8080"

	if got := healthURL(cfg); got != "http://127.0.0.1:8080/healthz" {
		t.Errorf("with encryption off the health check asks %q", got)
	}

	cfg.Web.TLS.Mode = config.TLSSelfSigned
	if got := healthURL(cfg); got != "https://127.0.0.1:8080/healthz" {
		t.Errorf("with encryption on the health check asks %q", got)
	}
}

// Nothing on this machine has the daemon's certificate in a trust store — that is what
// self-signed means — so the check trusts that certificate and only that one.
func TestTheHealthCheckTrustsTheCertificateTheDaemonServesWith(t *testing.T) {
	cfg := loadedConfig(t)
	cfg.Web.TLS.Mode = config.TLSSelfSigned

	pair := certificateForTest(t, selfSignedIn(cfg), "localhost", "127.0.0.1")
	daemon := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(healthyState))
	}))
	daemon.TLS = &tls.Config{Certificates: []tls.Certificate{pair}}
	daemon.StartTLS()
	defer daemon.Close()

	cfg.Web.Listen = daemon.Listener.Addr().String()
	state, err := fetchState(cfg)
	if err != nil {
		t.Fatalf("asking a daemon on https how it is: %v", err)
	}
	if state != healthyState {
		t.Errorf("the daemon said %q", state)
	}
}

// A certificate that is not the daemon's own is refused, or trusting the daemon's would prove
// nothing.
func TestTheHealthCheckRefusesACertificateThatIsNotTheDaemonsOwn(t *testing.T) {
	cfg := loadedConfig(t)
	cfg.Web.TLS.Mode = config.TLSSelfSigned

	certificateForTest(t, selfSignedIn(cfg), "localhost", "127.0.0.1")
	somebodyElse := certificateForTest(t, filepath.Join(t.TempDir(), "other.pem"), "localhost", "127.0.0.1")
	daemon := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(healthyState))
	}))
	daemon.TLS = &tls.Config{Certificates: []tls.Certificate{somebodyElse}}
	daemon.StartTLS()
	defer daemon.Close()

	cfg.Web.Listen = daemon.Listener.Addr().String()
	if _, err := fetchState(cfg); err == nil {
		t.Fatal("a certificate the daemon never had was accepted")
	}
}

// A certificate somebody provides is issued for the name they browse to, not for the 127.0.0.1
// the health check dials. Reading the name off the certificate is what keeps that case working.
func TestTheHealthCheckAcceptsAProvidedCertificateIssuedForAnotherName(t *testing.T) {
	cfg := loadedConfig(t)
	cfg.Web.TLS.Mode = config.TLSProvided
	cfg.Web.TLS.CertFile = filepath.Join(t.TempDir(), "provided.pem")

	pair := certificateForTest(t, cfg.Web.TLS.CertFile, "gpb.example.com")
	daemon := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(healthyState))
	}))
	daemon.TLS = &tls.Config{Certificates: []tls.Certificate{pair}}
	daemon.StartTLS()
	defer daemon.Close()

	cfg.Web.Listen = daemon.Listener.Addr().String()
	state, err := fetchState(cfg)
	if err != nil {
		t.Fatalf("asking a daemon behind a certificate for another name: %v", err)
	}
	if state != healthyState {
		t.Errorf("the daemon said %q", state)
	}
}

// selfSignedIn is where internal/web writes the certificate it makes for itself.
func selfSignedIn(cfg config.Config) string {
	return filepath.Join(cfg.TLSDir(), "self-signed.pem")
}

func loadedConfig(t *testing.T) config.Config {
	t.Helper()

	cfg, err := config.Load(t.TempDir())
	if err != nil {
		t.Fatalf("loading a scratch config: %v", err)
	}
	return cfg
}

// certificateForTest writes a self-signed pair where the daemon would have written one, and
// hands back the pair to serve with. The real one is made by internal/web at startup; this is
// the smallest thing that stands where it stands.
func certificateForTest(t *testing.T, certFile string, names ...string) tls.Certificate {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("making a key: %v", err)
	}
	template := x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "localhost"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	for _, name := range names {
		if address := net.ParseIP(name); address != nil {
			template.IPAddresses = append(template.IPAddresses, address)
		} else {
			template.DNSNames = append(template.DNSNames, name)
		}
	}
	encoded, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("making a certificate: %v", err)
	}

	if err := os.MkdirAll(filepath.Dir(certFile), 0o700); err != nil {
		t.Fatalf("making %s: %v", filepath.Dir(certFile), err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: encoded})
	if err := os.WriteFile(certFile, certPEM, 0o644); err != nil {
		t.Fatalf("writing the certificate: %v", err)
	}

	secret, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("encoding the key: %v", err)
	}
	pair, err := tls.X509KeyPair(certPEM, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: secret}))
	if err != nil {
		t.Fatalf("pairing the certificate and key: %v", err)
	}
	return pair
}
