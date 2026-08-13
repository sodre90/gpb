package web

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"gpb/internal/config"
)

const reachedAt = "https://192.168.1.10:8090"

func selfSigning(t *testing.T, cfg config.Config) config.Config {
	t.Helper()

	cfg.Web.TLS = config.TLS{Mode: config.TLSSelfSigned}
	cfg.Web.ExternalURL = reachedAt
	return cfg
}

// The whole promise of this setting is one connection a browser can make, so this makes one:
// a real listener, a real handshake, and a client that trusts nothing but the certificate the
// daemon just wrote for itself.
func TestTheUIAnswersOverHTTPSWithACertificateItMakesForItself(t *testing.T) {
	server, _ := testServer(t)
	server.cfg = selfSigning(t, server.cfg)
	server.cfg.Web.Listen = "127.0.0.1:0"

	site := httptest.NewUnstartedServer(server.Handler())
	certFile, keyFile, err := certificateFor(server.cfg, time.Now())
	if err != nil {
		t.Fatalf("making a certificate: %v", err)
	}
	pair, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		t.Fatalf("loading the pair back: %v", err)
	}
	site.TLS = &tls.Config{Certificates: []tls.Certificate{pair}}
	site.StartTLS()
	defer site.Close()

	response, err := clientTrusting(t, certFile).Get(site.URL + "/login")
	if err != nil {
		t.Fatalf("asking for the login page over https: %v", err)
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		t.Fatalf("the login page came back %d over https", response.StatusCode)
	}
	body, _ := io.ReadAll(response.Body)
	if !strings.Contains(string(body), "password") {
		t.Error("what came back over https is not the login page")
	}
}

// clientTrusting is a browser that has been shown this one certificate and no other, which is
// what accepting the warning amounts to. Trusting everything would make the test pass against a
// certificate for the wrong name.
func clientTrusting(t *testing.T, certFile string) *http.Client {
	t.Helper()

	encoded, err := os.ReadFile(certFile)
	if err != nil {
		t.Fatalf("reading the certificate: %v", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(encoded) {
		t.Fatal("the certificate on disk is not a PEM certificate")
	}
	return &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}}}
}

// A certificate is only worth having for the address it is checked against, and inside a
// container the process cannot learn that address from anything but the setting.
func TestTheCertificateCoversTheAddressTheUserTypes(t *testing.T) {
	server, _ := testServer(t)
	server.cfg = selfSigning(t, server.cfg)

	certFile, keyFile, err := certificateFor(server.cfg, time.Now())
	if err != nil {
		t.Fatalf("making a certificate: %v", err)
	}

	reached, _ := url.Parse(reachedAt)
	for _, name := range []string{reached.Hostname(), "localhost", "127.0.0.1"} {
		if err := parsedCertificate(t, certFile).VerifyHostname(name); err != nil {
			t.Errorf("the certificate is not valid for %s: %v", name, err)
		}
	}
	if err := parsedCertificate(t, certFile).VerifyHostname("192.168.1.99"); err == nil {
		t.Error("the certificate is valid for an address nobody asked for, so it proves nothing")
	}

	// The key is the whole of the secret. The data dir is 0700 for the Chrome profile's sake and
	// this belongs under exactly the same roof.
	info, err := os.Stat(keyFile)
	if err != nil {
		t.Fatalf("reading the key back: %v", err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("the private key is mode %04o, want 0600", mode)
	}
}

func parsedCertificate(t *testing.T, certFile string) *x509.Certificate {
	t.Helper()

	encoded, err := os.ReadFile(certFile)
	if err != nil {
		t.Fatalf("reading %s: %v", certFile, err)
	}
	block, _ := pem.Decode(encoded)
	if block == nil {
		t.Fatalf("%s is not a PEM certificate", certFile)
	}
	parsed, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("parsing %s: %v", certFile, err)
	}
	return parsed
}

// Making a new certificate every start would undo the one thing the user does about the warning:
// the exception they added is for this certificate, not for this address.
func TestARestartKeepsTheCertificateItAlreadyHas(t *testing.T) {
	server, _ := testServer(t)
	server.cfg = selfSigning(t, server.cfg)

	certFile, _, err := certificateFor(server.cfg, time.Now())
	if err != nil {
		t.Fatalf("making a certificate: %v", err)
	}
	first := parsedCertificate(t, certFile).SerialNumber.String()

	if _, _, err := certificateFor(server.cfg, time.Now().Add(24*time.Hour)); err != nil {
		t.Fatalf("starting again: %v", err)
	}
	if second := parsedCertificate(t, certFile).SerialNumber.String(); second != first {
		t.Error("a restart wrote a new certificate, so every browser that accepted the old one asks again")
	}
}

// The two ways a certificate stops being the right one: the address it was made for changed, and
// time ran out. Both are noticed at a start, which is the only moment this program looks.
func TestTheCertificateIsRemadeWhenItNoLongerFits(t *testing.T) {
	server, _ := testServer(t)
	server.cfg = selfSigning(t, server.cfg)

	certFile, _, err := certificateFor(server.cfg, time.Now())
	if err != nil {
		t.Fatalf("making a certificate: %v", err)
	}
	first := parsedCertificate(t, certFile).SerialNumber.String()

	moved := server.cfg
	moved.Web.ExternalURL = "https://photos.lan:8090"
	if _, _, err := certificateFor(moved, time.Now()); err != nil {
		t.Fatalf("remaking it for the new address: %v", err)
	}
	afterMove := parsedCertificate(t, certFile)
	if afterMove.SerialNumber.String() == first {
		t.Fatal("the address changed and the certificate did not, so the browser refuses the new one")
	}
	if err := afterMove.VerifyHostname("photos.lan"); err != nil {
		t.Errorf("the remade certificate is not valid for the new address: %v", err)
	}

	// A month before it runs out, not the day after: an appliance nobody restarts should never
	// serve an expired certificate.
	if _, _, err := certificateFor(moved, time.Now().Add(certificateLife-renewWithin+time.Hour)); err != nil {
		t.Fatalf("renewing it: %v", err)
	}
	if renewed := parsedCertificate(t, certFile).SerialNumber.String(); renewed == afterMove.SerialNumber.String() {
		t.Error("a certificate a month from running out was not renewed")
	}
}

// The daemon that cannot get a certificate must not quietly serve plain http instead. Turning
// encryption off without being asked is the one failure whose whole point is that nobody notices.
func TestAMissingCertificateStopsTheServerRatherThanDowngradingIt(t *testing.T) {
	server, _ := testServer(t)
	server.cfg.Web.TLS = config.TLS{
		Mode:     config.TLSProvided,
		CertFile: t.TempDir() + "/nothing.pem",
		KeyFile:  t.TempDir() + "/nothing.key",
	}

	if _, _, err := certificateFor(server.cfg, time.Now()); err == nil {
		t.Fatal("a certificate that is not there was accepted")
	}

	// In a goroutine because the failure being guarded against is a server that serves: falling
	// back to plain http would block here rather than come back, and a test that hangs says far
	// less than one that says what went wrong.
	refused := make(chan error, 1)
	go func() { refused <- server.serve(&http.Server{Addr: "127.0.0.1:0"}) }()
	select {
	case err := <-refused:
		if err == nil {
			t.Error("the server came back without an error, having served nothing")
		}
	case <-time.After(5 * time.Second):
		t.Error("the server is listening with no certificate, which means it is serving plain http")
	}
}

// The cookie is a bearer token for the same account the password is. Marking it Secure is free
// once the connection is encrypted -- and would lock the user out of a page served over http,
// which is why it is not simply always on.
func TestTheSessionCookieIsSecureOnlyWhenTheConnectionIs(t *testing.T) {
	for _, mode := range []struct {
		tls  config.TLS
		want bool
	}{
		{config.TLS{Mode: config.TLSOff}, false},
		{config.TLS{Mode: config.TLSSelfSigned}, true},
	} {
		server, _ := testServer(t)
		server.cfg.Web.TLS = mode.tls
		handler := server.Handler()

		cookie := login(t, handler)
		if cookie.Secure != mode.want {
			t.Errorf("with encryption %q the session cookie has Secure=%v, want %v",
				mode.tls.Mode, cookie.Secure, mode.want)
		}
		if !cookie.HttpOnly {
			t.Errorf("with encryption %q the session cookie is not HttpOnly", mode.tls.Mode)
		}
	}
}
