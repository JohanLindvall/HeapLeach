package httpx

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// testPKI is a root, an intermediate it signed, and a server certificate the
// intermediate signed, naming its issuer's address the way real ones do.
type testPKI struct {
	roots   *x509.CertPool
	inter   *x509.Certificate
	leaf    *x509.Certificate
	leafKey *ecdsa.PrivateKey
}

func newTestPKI(t *testing.T, aia string) *testPKI {
	t.Helper()
	now := time.Now()
	issue := func(tmpl, parent *x509.Certificate, pub, signer any) *x509.Certificate {
		der, err := x509.CreateCertificate(rand.Reader, tmpl, parent, pub, signer)
		if err != nil {
			t.Fatal(err)
		}
		cert, err := x509.ParseCertificate(der)
		if err != nil {
			t.Fatal(err)
		}
		return cert
	}
	key := func() *ecdsa.PrivateKey {
		k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		return k
	}
	rootKey, interKey, leafKey := key(), key(), key()
	ca := func(serial int64, name string) *x509.Certificate {
		return &x509.Certificate{
			SerialNumber: big.NewInt(serial), Subject: pkix.Name{CommonName: name},
			NotBefore: now.Add(-time.Hour), NotAfter: now.Add(24 * time.Hour),
			IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
		}
	}
	root := issue(ca(1, "Test Root"), ca(1, "Test Root"), &rootKey.PublicKey, rootKey)
	inter := issue(ca(2, "Test Intermediate"), root, &interKey.PublicKey, rootKey)
	leaf := issue(&x509.Certificate{
		SerialNumber: big.NewInt(3), Subject: pkix.Name{CommonName: "example.test"},
		DNSNames: []string{"example.test"}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(24 * time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IssuingCertificateURL: []string{aia},
	}, inter, &leafKey.PublicKey, interKey)

	roots := x509.NewCertPool()
	roots.AddCert(root)
	return &testPKI{roots: roots, inter: inter, leaf: leaf, leafKey: leafKey}
}

// completer is a chainCompleter trusting pki's root, whose AIA fetches are
// answered by serve and counted.
func completer(pki *testPKI, serve func() []byte) (*chainCompleter, *atomic.Int32) {
	var fetches atomic.Int32
	return &chainCompleter{
		roots:  pki.roots,
		issuer: make(map[string]*x509.Certificate),
		fetch: func(context.Context, string) ([]byte, error) {
			fetches.Add(1)
			return serve(), nil
		},
	}, &fetches
}

func TestAWholeChainVerifiesWithoutFetching(t *testing.T) {
	pki := newTestPKI(t, "http://aia.example.test/inter.der")
	c, fetches := completer(pki, func() []byte { return pki.inter.Raw })
	if err := c.verify("example.test", []*x509.Certificate{pki.leaf, pki.inter}); err != nil {
		t.Fatalf("whole chain: %v", err)
	}
	if fetches.Load() != 0 {
		t.Errorf("fetched %d issuers for a chain that was whole", fetches.Load())
	}
}

// The case that motivates it: a server sending only its own certificate.
func TestAShortChainIsCompletedFromItsIssuerAddressOnce(t *testing.T) {
	pki := newTestPKI(t, "http://aia.example.test/inter.der")
	c, fetches := completer(pki, func() []byte { return pki.inter.Raw })
	for range 2 {
		if err := c.verify("example.test", []*x509.Certificate{pki.leaf}); err != nil {
			t.Fatalf("short chain: %v", err)
		}
	}
	if fetches.Load() != 1 {
		t.Errorf("fetched the issuer %d times, want once and then remembered", fetches.Load())
	}
}

// Completion never relaxes anything: the wrong name is still the wrong name,
// and an issuer that did not sign the certificate completes nothing.
func TestCompletionChangesNothingAboutTrust(t *testing.T) {
	pki := newTestPKI(t, "http://aia.example.test/inter.der")
	c, fetches := completer(pki, func() []byte { return pki.inter.Raw })
	var hostname x509.HostnameError
	if err := c.verify("other.test", []*x509.Certificate{pki.leaf}); !errors.As(err, &hostname) {
		t.Errorf("wrong name: %v, want a hostname error", err)
	}
	if fetches.Load() != 0 {
		t.Error("a wrong name sent the completer fetching")
	}

	stranger := newTestPKI(t, "http://aia.example.test/other.der")
	c, _ = completer(pki, func() []byte { return stranger.inter.Raw })
	var unknown x509.UnknownAuthorityError
	if err := c.verify("example.test", []*x509.Certificate{pki.leaf}); !errors.As(err, &unknown) {
		t.Errorf("unrelated issuer: %v, want the chain still unknown", err)
	}

	// No address, or one that is not http(s), is nothing to fetch.
	c, fetches = completer(pki, func() []byte { return pki.inter.Raw })
	for _, aia := range [][]string{nil, {"ldap://ca.example.test/inter"}} {
		leaf := *pki.leaf
		leaf.IssuingCertificateURL = aia
		if err := c.verify("example.test", []*x509.Certificate{&leaf}); err == nil {
			t.Errorf("issuer address %v: accepted", aia)
		}
	}
	if fetches.Load() != 0 {
		t.Errorf("fetched %d times from addresses that are not http", fetches.Load())
	}
}

// End to end, through both of the client's TLS paths: a server presenting
// its certificate alone is reached once the issuer is fetched from the
// address the certificate names.
func TestTheClientCompletesAShortChain(t *testing.T) {
	var issuerHits atomic.Int32
	var pki *testPKI
	aia := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		issuerHits.Add(1)
		_, _ = w.Write(pki.inter.Raw)
	}))
	defer aia.Close()
	pki = newTestPKI(t, aia.URL+"/inter.der")

	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "served")
	}))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{{
		Certificate: [][]byte{pki.leaf.Raw}, // the intermediate left out
		PrivateKey:  pki.leafKey,
	}}}
	srv.EnableHTTP2 = true
	srv.StartTLS()
	defer srv.Close()

	saved := chains
	chains = &chainCompleter{roots: pki.roots, fetch: fetchIssuer, issuer: make(map[string]*x509.Certificate)}
	defer func() { chains = saved }()

	for _, utls := range []string{"0", "1"} {
		t.Setenv("HEAPLEACH_UTLS", utls)
		client := New("test-agent", "en-US", 0, 5*time.Second)
		body, err := client.GetString(context.Background(), srv.URL+"/", nil)
		if err != nil || body != "served" {
			t.Errorf("HEAPLEACH_UTLS=%s: %q, %v", utls, body, err)
		}
	}
	if issuerHits.Load() != 1 {
		t.Errorf("the issuer was fetched %d times across both paths, want once", issuerHits.Load())
	}
}

// Both TLS paths turn off crypto/tls's own check and verify in its place, so
// each is held to it on its own: the Chrome-shaped dial completes a short
// chain itself rather than by falling back, and neither path accepts a
// certificate whose chain leads to no trusted root.
func TestBothTLSPathsVerifyOnTheirOwn(t *testing.T) {
	var pki *testPKI
	aia := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(pki.inter.Raw)
	}))
	defer aia.Close()
	pki = newTestPKI(t, aia.URL+"/inter.der")

	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{pki.leaf.Raw}, PrivateKey: pki.leafKey}}}
	srv.EnableHTTP2 = true
	srv.StartTLS()
	defer srv.Close()
	addr := srv.Listener.Addr().String()

	saved := chains
	defer func() { chains = saved }()

	chains = &chainCompleter{roots: pki.roots, fetch: fetchIssuer, issuer: make(map[string]*x509.Certificate)}
	conn, err := dialChrome(context.Background(), "tcp", addr)
	if err != nil {
		t.Fatalf("Chrome-shaped dial to a short chain: %v", err)
	}
	conn.Close()

	// Trusting some other root, the same server must be refused by both.
	stranger := newTestPKI(t, "http://aia.example.test/x")
	chains = &chainCompleter{roots: stranger.roots, fetch: fetchIssuer, issuer: make(map[string]*x509.Certificate)}
	if conn, err := dialChrome(context.Background(), "tcp", addr); err == nil {
		conn.Close()
		t.Error("the Chrome-shaped dial accepted a chain leading to no trusted root")
	}
	t.Setenv("HEAPLEACH_UTLS", "0")
	if _, err := New("test-agent", "en-US", 0, 5*time.Second).GetString(context.Background(), srv.URL+"/", nil); err == nil {
		t.Error("the standard path accepted a chain leading to no trusted root")
	}
}
