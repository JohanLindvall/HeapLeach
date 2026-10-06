// SPDX-License-Identifier: MIT

package httpx

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sync"
	"time"
)

// Completing a certificate chain the server left short.
//
// A server is meant to send its own certificate and every intermediate up to
// a trusted root. Some send only their own — vids.st leaves out Sectigo's
// intermediate — and browsers cope by fetching the missing issuer from the
// address the certificate itself names (Authority Information Access, "CA
// Issuers"). So such a site works in a browser and fails everywhere else with
// "certificate signed by unknown authority": Go does no such fetching, and
// neither does curl. This does it.
//
// Nothing about trust changes. The verification is crypto/x509's own — the
// system's roots, the hostname, the validity period — and it runs on every
// connection, for both the browser-shaped handshake and the standard one. A
// fetched intermediate only completes a chain: it is accepted if it signs the
// certificate in front of it and itself leads to a trusted root, which a
// forged one cannot. It is fetched only when the ordinary check failed for
// want of an issuer, a few hops at most, from http or https addresses only,
// and kept for the life of the process.

const (
	// aiaMaxHops bounds how many missing issuers are fetched for one chain.
	// One is the ordinary case: the server sent its certificate and nothing
	// else, and the intermediate it left out leads straight to a root.
	aiaMaxHops = 3
	// aiaMaxBytes bounds one fetched certificate; real ones are a couple of
	// kilobytes.
	aiaMaxBytes = 64 << 10
	aiaTimeout  = 10 * time.Second
)

// chainCompleter verifies presented chains, completing them from AIA.
type chainCompleter struct {
	// roots is what chains must lead to; nil is the system's pool. Set only
	// by tests, which cannot put their own root in the system's.
	roots *x509.CertPool
	fetch func(ctx context.Context, link string) ([]byte, error)

	mu     sync.Mutex
	issuer map[string]*x509.Certificate // by AIA address
}

// chains is shared by every client: what one connection learned about a
// host's missing intermediate saves the next one the fetch.
var chains = &chainCompleter{fetch: fetchIssuer, issuer: make(map[string]*x509.Certificate)}

// verify checks a presented chain the way crypto/tls would, completing it
// from the certificates' own AIA addresses when the server left an issuer
// out. serverName is the name the connection was made for.
func (c *chainCompleter) verify(serverName string, presented []*x509.Certificate) error {
	if len(presented) == 0 {
		return errors.New("tls: the server presented no certificate")
	}
	leaf := presented[0]
	pool := x509.NewCertPool()
	for _, cert := range presented[1:] {
		pool.AddCert(cert)
	}
	opts := x509.VerifyOptions{DNSName: serverName, Roots: c.roots, Intermediates: pool}

	_, err := leaf.Verify(opts)
	// Only a chain that stops short is completed. Any other failure — the
	// wrong name, an expired certificate — is the answer as it stands.
	next := leaf
	for hop := 0; err != nil && hop < aiaMaxHops; hop++ {
		var unknown x509.UnknownAuthorityError
		if !errors.As(err, &unknown) {
			break
		}
		issuer := c.issuerOf(next)
		if issuer == nil {
			break
		}
		pool.AddCert(issuer)
		next = issuer
		_, err = leaf.Verify(opts)
	}
	return err
}

// issuerOf returns the certificate cert names as its issuer's, fetched from
// its AIA addresses or remembered from an earlier fetch, or nil.
func (c *chainCompleter) issuerOf(cert *x509.Certificate) *x509.Certificate {
	for _, link := range cert.IssuingCertificateURL {
		c.mu.Lock()
		known := c.issuer[link]
		c.mu.Unlock()
		if known != nil {
			return known
		}

		u, err := url.Parse(link)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), aiaTimeout)
		body, err := c.fetch(ctx, link)
		cancel()
		if err != nil {
			continue
		}
		issuer, err := parseIssuer(body)
		if err != nil {
			continue
		}
		c.mu.Lock()
		c.issuer[link] = issuer
		c.mu.Unlock()
		return issuer
	}
	return nil
}

// parseIssuer reads a certificate served as DER, which is what CA Issuers
// addresses serve, or as PEM, which some do anyway.
func parseIssuer(body []byte) (*x509.Certificate, error) {
	if block, _ := pem.Decode(body); block != nil && block.Type == "CERTIFICATE" {
		body = block.Bytes
	}
	return x509.ParseCertificate(body)
}

// aiaClient fetches issuers. A plain client of its own: the fetch happens in
// the middle of another handshake, and must not go through the transport
// that is waiting on it.
var aiaClient = &http.Client{
	Timeout:   aiaTimeout,
	Transport: &http.Transport{Proxy: http.ProxyFromEnvironment},
}

// fetchIssuer downloads one issuer certificate.
func fetchIssuer(ctx context.Context, link string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, link, nil)
	if err != nil {
		return nil, err
	}
	resp, err := aiaClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("issuer %s: %s", link, resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, aiaMaxBytes+1))
	if err != nil {
		return nil, err
	}
	if len(body) > aiaMaxBytes {
		return nil, fmt.Errorf("issuer %s is larger than a certificate", link)
	}
	return body, nil
}
