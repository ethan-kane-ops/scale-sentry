package loadgen

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestBuildTLSConfig_Defaults(t *testing.T) {
	cfg := Config{ConnectionMode: KeepAlive}
	tlsCfg, err := buildTLSConfig(cfg)
	if err != nil {
		t.Fatalf("buildTLSConfig: %v", err)
	}
	if tlsCfg.MinVersion != 0x0303 { // TLS 1.2
		t.Errorf("MinVersion = %#x, want TLS 1.2", tlsCfg.MinVersion)
	}
	if tlsCfg.InsecureSkipVerify {
		t.Error("InsecureSkipVerify default should be false")
	}
	if tlsCfg.RootCAs != nil {
		t.Error("RootCAs should be nil when no CA bundle is configured (use system pool)")
	}
}

func TestBuildTLSConfig_InsecureSkip(t *testing.T) {
	tlsCfg, err := buildTLSConfig(Config{TLSInsecureSkipVerify: true})
	if err != nil {
		t.Fatalf("buildTLSConfig: %v", err)
	}
	if !tlsCfg.InsecureSkipVerify {
		t.Error("InsecureSkipVerify not propagated")
	}
}

func TestBuildTLSConfig_CABundle(t *testing.T) {
	dir := t.TempDir()
	bundle := filepath.Join(dir, "ca.pem")
	if err := os.WriteFile(bundle, generateSelfSignedCAPEM(t), 0o600); err != nil {
		t.Fatalf("write bundle: %v", err)
	}
	tlsCfg, err := buildTLSConfig(Config{TLSCABundlePath: bundle})
	if err != nil {
		t.Fatalf("buildTLSConfig: %v", err)
	}
	if tlsCfg.RootCAs == nil {
		t.Fatal("RootCAs nil, want loaded pool from bundle")
	}
}

func TestBuildTLSConfig_BadBundle(t *testing.T) {
	dir := t.TempDir()
	bundle := filepath.Join(dir, "junk.pem")
	if err := os.WriteFile(bundle, []byte("not a pem"), 0o600); err != nil {
		t.Fatalf("write bundle: %v", err)
	}
	_, err := buildTLSConfig(Config{TLSCABundlePath: bundle})
	if err == nil || !strings.Contains(err.Error(), "no valid PEM") {
		t.Errorf("err = %v, want PEM validation error", err)
	}
}

func TestBuildTLSConfig_MissingBundle(t *testing.T) {
	_, err := buildTLSConfig(Config{TLSCABundlePath: "/nonexistent/path"})
	if err == nil || !strings.Contains(err.Error(), "read TLS CA bundle") {
		t.Errorf("err = %v, want read error", err)
	}
}

// generateSelfSignedCAPEM creates a one-off self-signed CA cert and
// returns it PEM-encoded. Used by the TLS bundle tests so they don't
// depend on a checked-in fixture.
func generateSelfSignedCAPEM(t *testing.T) []byte {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("gen key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "scale-sentry test CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}
