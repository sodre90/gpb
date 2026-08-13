package web

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"time"

	"gpb/internal/config"
)

// certificateFor answers with the pair the server should serve with, making one first if the
// mode is self-signed and what is on disk will not do. A provided certificate is the user's to
// keep working; this only checks it loads, so that a typo shows up here rather than as a daemon
// that will not start.
func certificateFor(cfg config.Config, now time.Time) (certFile, keyFile string, err error) {
	switch cfg.Web.TLS.Mode {
	case config.TLSProvided:
		certFile, keyFile = cfg.Web.TLS.CertFile, cfg.Web.TLS.KeyFile
		if _, err := tls.LoadX509KeyPair(certFile, keyFile); err != nil {
			return "", "", fmt.Errorf("reading the certificate and key: %w", err)
		}
		return certFile, keyFile, nil
	case config.TLSSelfSigned:
		return ensureSelfSigned(cfg, now)
	}
	return "", "", fmt.Errorf("web.tls.mode %q asks for no certificate", cfg.Web.TLS.Mode)
}

func selfSignedPaths(cfg config.Config) (certFile, keyFile string) {
	dir := cfg.TLSDir()
	return filepath.Join(dir, "self-signed.pem"), filepath.Join(dir, "self-signed.key")
}

func ensureSelfSigned(cfg config.Config, now time.Time) (certFile, keyFile string, err error) {
	certFile, keyFile = selfSignedPaths(cfg)
	names := namesToCertify(cfg)

	if reason := whyUnusable(certFile, names, now); reason == "" {
		return certFile, keyFile, nil
	} else if err := writeSelfSigned(certFile, keyFile, names, now); err != nil {
		return "", "", fmt.Errorf("making a certificate (%s): %w", reason, err)
	}
	return certFile, keyFile, nil
}

// namesToCertify is what a browser checks the certificate against, and getting it wrong is the
// whole difference between a warning the user can accept and one they cannot. The address the
// user types is the only one this process can know — inside a container its own hostname is a
// random hex string and its own addresses are on a network nobody browses from — so it comes
// from external_url, which is the same setting the sign-in links are built out of.
func namesToCertify(cfg config.Config) []string {
	names := []string{"localhost", "127.0.0.1", "::1"}
	if reached, err := url.Parse(cfg.Web.ExternalURL); err == nil && reached.Hostname() != "" {
		names = append(names, reached.Hostname())
	}
	slices.Sort(names)
	return slices.Compact(names)
}

// CertificateServed is the certificate the daemon presents, for anything on this machine that
// dials the daemon and has to be sure of what answered — chiefly `gpb status`, which is the
// container's health command and cannot rely on a trust store for a certificate that is in none.
func CertificateServed(cfg config.Config) (*x509.Certificate, error) {
	certFile := cfg.Web.TLS.CertFile
	if cfg.Web.TLS.Mode == config.TLSSelfSigned {
		certFile, _ = selfSignedPaths(cfg)
	}
	return parseCertificate(certFile)
}

func parseCertificate(certFile string) (*x509.Certificate, error) {
	encoded, err := os.ReadFile(certFile)
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(encoded)
	if block == nil {
		return nil, fmt.Errorf("%s is not a PEM certificate", certFile)
	}
	return x509.ParseCertificate(block.Bytes)
}

// whyUnusable says what is wrong with the certificate already on disk, in words fit for the log
// line that reports it being replaced, and "" when there is nothing wrong with it.
func whyUnusable(certFile string, names []string, now time.Time) string {
	certificate, err := parseCertificate(certFile)
	if errors.Is(err, os.ErrNotExist) {
		return "there is not one yet"
	} else if err != nil {
		return "the one on disk cannot be read as a certificate"
	}

	if now.After(certificate.NotAfter.Add(-renewWithin)) {
		return fmt.Sprintf("the one on disk runs out on %s", certificate.NotAfter.Format(time.DateOnly))
	}
	for _, name := range names {
		if certificate.VerifyHostname(name) != nil {
			return fmt.Sprintf("the one on disk is not valid for %s", name)
		}
	}
	return ""
}

func writeSelfSigned(certFile, keyFile string, names []string, now time.Time) error {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return err
	}

	template := x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: names[len(names)-1], Organization: []string{"gpb"}},
		// A little before now, because the clock on the machine that made this and the clock on
		// the machine reading it are not the same clock.
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(certificateLife),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment | x509.KeyUsageCertSign,
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
		return err
	}
	if err := os.MkdirAll(filepath.Dir(certFile), 0o700); err != nil {
		return err
	}
	if err := writePEM(certFile, 0o644, &pem.Block{Type: "CERTIFICATE", Bytes: encoded}); err != nil {
		return err
	}

	secret, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return err
	}
	return writePEM(keyFile, 0o600, &pem.Block{Type: "EC PRIVATE KEY", Bytes: secret})
}

func writePEM(path string, mode os.FileMode, block *pem.Block) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if err := pem.Encode(file, block); err != nil {
		file.Close()
		return err
	}
	return file.Close()
}

const (
	// 397 days because Safari refuses anything longer outright, and somebody who gets tired of
	// clicking through the warning will put this in their trust store — where that rule applies
	// to a certificate made here exactly as it does to one from a public CA.
	certificateLife = 397 * 24 * time.Hour
	// Renewed at startup while there is still a month in it, so the daemon has to go a year and a
	// month without a restart before anyone meets an expired one. The settings page shows the
	// date for the same reason.
	renewWithin = 30 * 24 * time.Hour
)

// certificateExpiry is what the settings page reports. A self-signed certificate renews itself at
// startup, so this is mostly reassurance; a provided one is the user's to replace, and the date
// is the only warning they will get.
func certificateExpiry(certFile string) (time.Time, error) {
	certificate, err := parseCertificate(certFile)
	if err != nil {
		return time.Time{}, err
	}
	return certificate.NotAfter, nil
}
