// SPDX-License-Identifier: MIT

// Package proxy adapts proxytron's persistent lease pool to the downloader.
package proxy

import (
	"cmp"
	"context"
	"errors"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"net/url"

	"github.com/JohanLindvall/HeapLeach/internal/config"
	"github.com/JohanLindvall/HeapLeach/internal/httpx"
	"github.com/JohanLindvall/proxytron"
	"github.com/JohanLindvall/proxytron/lease"
)

const Direct = lease.Direct

type Pool = lease.Pool
type Lease = lease.Lease
type Configuration = lease.Configuration
type Query = lease.Query
type Page = lease.Page
type ExitProber = lease.ExitProber

var (
	ErrExitInUse       = lease.ErrExitInUse
	ErrExitProbeFailed = lease.ErrExitProbeFailed
)

// protocols maps a protocol a group can be held to onto proxytron's.
var protocols = map[string]proxytron.ProxyType{
	"": proxytron.ProxyTypeAny, config.ProxyTypeAny: proxytron.ProxyTypeAny,
	config.ProxyTypeSOCKS: proxytron.ProxyTypeSOCKS, config.ProxyTypeHTTP: proxytron.ProxyTypeHTTP,
}

// SetTypes holds each group's routes to its protocol: its own where groups
// names one, otherwise all's. A specific protocol leaves the direct
// connection out of the group too, and a group with no route of its
// protocol gets none rather than another protocol's.
func SetTypes(p *Pool, all string, groups map[string]string) error {
	for _, group := range config.ProxyGroups {
		kind := cmp.Or(groups[group], all)
		t, ok := protocols[kind]
		if !ok {
			return config.CheckProxyType(kind)
		}
		if err := p.SetScopeProxyType(group, t); err != nil {
			return err
		}
	}
	return nil
}

func ParseConfiguration(endpoints, feeds []string) (Configuration, error) {
	c, err := lease.ParseConfiguration(endpoints, feeds)
	if err != nil {
		return c, err
	}
	for _, raw := range c.Endpoints {
		if raw == Direct {
			continue
		}
		if !supported(raw) {
			return c, errors.New("unsupported proxy protocol")
		}
	}
	return c, nil
}

// supported reports an endpoint of a protocol the pool takes, whether
// configured or found in a feed.
func supported(endpoint string) bool {
	u, err := url.Parse(endpoint)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https" || u.Scheme == "socks5h")
}

func EmptyPage(q Query) Page { return lease.EmptyPage(q) }

func Open(path string, endpoints, feeds []string, client *httpx.Client, log *slog.Logger) (*Pool, error) {
	c, err := ParseConfiguration(endpoints, feeds)
	if err != nil {
		return nil, err
	}
	return lease.Open(path, c.Endpoints, c.Feeds, clientAdapter{Client: client}, log,
		lease.WithHealthEpoch(config.ProxyHealthEpoch), lease.WithExitTTL(config.ProxyExitTTL),
		lease.WithFeedFilter(supported),
		lease.WithPolicy(lease.Policy{RecoveryScopes: []string{config.GroupWAF}, CappedScopes: []string{config.GroupKeep2Share, config.GroupEmload}}),
		lease.WithChallengeClassifier(func(err error) bool { var challenge *httpx.ChallengeError; return errors.As(err, &challenge) }))
}

type clientAdapter struct {
	*httpx.Client
	agent string
}

func (c clientAdapter) ThroughProxy(endpoint string) (lease.Client, error) {
	client, err := c.Client.ThroughProxy(endpoint)
	if err != nil {
		return nil, err
	}
	return clientAdapter{Client: client, agent: config.BrowserUserAgents[rand.IntN(len(config.BrowserUserAgents))]}, nil
}
func (c clientAdapter) WithRoute(ctx context.Context, id, scope string, observe func(lease.Observation)) context.Context {
	agent := c.agent
	if scope == config.GroupWAF {
		agent = ""
	}
	return httpx.WithRouteAgent(ctx, id, c.Client, func(o httpx.RouteObservation) {
		var challenge *httpx.ChallengeError
		observe(lease.Observation{Duration: o.Duration, Bytes: o.Bytes, Status: o.Status, Err: o.Err, RetryAfter: o.RetryAfter, Challenged: errors.As(o.Err, &challenge)})
	}, agent)
}

func (c clientAdapter) NewRequest(ctx context.Context, method, url string, body io.Reader) (*http.Request, error) {
	var b []byte
	if body != nil {
		var err error
		b, err = io.ReadAll(body)
		if err != nil {
			return nil, err
		}
	}
	return c.Client.NewRequest(ctx, method, url, b)
}
