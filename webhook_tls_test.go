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
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestWebhookTLSIsOptIn(t *testing.T) {
	httpURL, err := url.Parse("http://example.com/reload")
	if err != nil {
		t.Fatal(err)
	}
	client, err := newWebhookClient(time.Second, webhookTLSOptions{}, []*url.URL{httpURL})
	if err != nil || client.Transport != nil || client.CheckRedirect != nil {
		t.Fatalf("default HTTP client changed: client=%+v, err=%v", client, err)
	}

	httpsURL, err := url.Parse("https://example.com/reload")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name     string
		options  webhookTLSOptions
		webhooks []*url.URL
	}{
		{"missing key", webhookTLSOptions{certFile: "cert.pem"}, []*url.URL{httpsURL}},
		{"missing certificate", webhookTLSOptions{keyFile: "key.pem"}, []*url.URL{httpsURL}},
		{"HTTP endpoint", webhookTLSOptions{caFile: "ca.pem"}, []*url.URL{httpURL}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := newWebhookClient(time.Second, tc.options, tc.webhooks); err == nil {
				t.Fatal("expected invalid TLS configuration to be rejected")
			}
		})
	}

	invalidCA := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(invalidCA, []byte("not a PEM certificate"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := newWebhookClient(time.Second, webhookTLSOptions{caFile: invalidCA}, []*url.URL{httpsURL}); err == nil {
		t.Fatal("expected invalid CA file to be rejected")
	}
}

func TestWebhookMTLSReloadsMountedCertificates(t *testing.T) {
	clientCA, clientCAKey, clientCAPEM := testCertificateAuthority(t)
	clientCAPool := x509.NewCertPool()
	if !clientCAPool.AppendCertsFromPEM(clientCAPEM) {
		t.Fatal("could not add client CA")
	}
	var observed []string
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.TLS == nil || len(r.TLS.PeerCertificates) != 1 {
			t.Error("request did not present a client certificate")
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		observed = append(observed, r.TLS.PeerCertificates[0].Subject.CommonName)
		w.WriteHeader(http.StatusOK)
	}))
	server.TLS = &tls.Config{ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: clientCAPool, MinVersion: tls.VersionTLS12}
	server.StartTLS()
	defer server.Close()

	serverCAPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	mount := t.TempDir()
	writeVersion := func(version, commonName string, caPEM []byte) {
		t.Helper()
		dir := filepath.Join(mount, version)
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
		certPEM, keyPEM := testClientCertificate(t, clientCA, clientCAKey, commonName)
		for name, contents := range map[string][]byte{
			"ca.crt": caPEM, "tls.crt": certPEM, "tls.key": keyPEM,
		} {
			if err := os.WriteFile(filepath.Join(dir, name), contents, 0600); err != nil {
				t.Fatal(err)
			}
		}
	}
	writeVersion("version-one", "first-client", serverCAPEM)
	writeVersion("version-two", "second-client", serverCAPEM)
	writeVersion("version-three", "third-client", clientCAPEM)
	writeVersion("version-four", "fourth-client", serverCAPEM)
	for _, name := range []string{"ca.crt", "tls.crt", "tls.key"} {
		if err := os.Symlink(filepath.Join("..data", name), filepath.Join(mount, name)); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("version-one", filepath.Join(mount, "..data")); err != nil {
		t.Fatal(err)
	}
	rotate := func(version string) {
		t.Helper()
		if err := os.Symlink(version, filepath.Join(mount, "..data_tmp")); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(filepath.Join(mount, "..data_tmp"), filepath.Join(mount, "..data")); err != nil {
			t.Fatal(err)
		}
	}

	webhookURL, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	client, err := newWebhookClient(time.Second, webhookTLSOptions{
		caFile: filepath.Join(mount, "ca.crt"), certFile: filepath.Join(mount, "tls.crt"), keyFile: filepath.Join(mount, "tls.key"),
	}, []*url.URL{webhookURL})
	if err != nil {
		t.Fatal(err)
	}
	request := func() {
		t.Helper()
		response, err := client.Post(server.URL, "", nil)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != http.StatusOK {
			t.Fatalf("got status %d, want 200", response.StatusCode)
		}
	}
	request()
	rotate("version-two")
	request()
	rotate("version-three")
	response, err := client.Post(server.URL, "", nil)
	if err == nil {
		response.Body.Close()
		t.Fatal("request succeeded with an untrusted rotated CA")
	}
	rotate("version-four")
	request()
	if len(observed) != 3 || observed[0] != "first-client" || observed[1] != "second-client" || observed[2] != "fourth-client" {
		t.Fatalf("client certificate did not rotate: %v", observed)
	}
}

func testCertificateAuthority(t *testing.T) (*x509.Certificate, *ecdsa.PrivateKey, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "configmap-reload test client CA"},
		NotBefore:    time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	certificate, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return certificate, key, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func testClientCertificate(t *testing.T, ca *x509.Certificate, caKey *ecdsa.PrivateKey, commonName string) ([]byte, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: serial, Subject: pkix.Name{CommonName: commonName},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, ca, &key.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
}
