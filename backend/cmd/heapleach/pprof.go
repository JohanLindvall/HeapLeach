// SPDX-License-Identifier: MIT

package main

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/pprof"
	"runtime"
	"time"

	"github.com/JohanLindvall/HeapLeach/internal/config"
)

// startProfiler serves Go's runtime profiles on a listener of their own, for
// the question a CPU figure cannot answer on its own: what the time is being
// spent on. `go tool pprof http://<addr>/debug/pprof/profile?seconds=30`
// reads one.
//
// A listener of its own rather than a route on the API, because the API
// listens on every interface and these describe the process from the inside
// — its goroutines, its heap, the command line it was started with. It is
// off unless an address is named, and an address that is not loopback is
// logged as such. In a container, 127.0.0.1 is the container's own and is
// reached with `docker exec`, which publishes nothing.
//
// The handlers are mounted on a mux of their own. Importing net/http/pprof
// also registers them on http.DefaultServeMux, which nothing here serves.
func startProfiler(addr string, log *slog.Logger) (stop func(), err error) {
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("profiling: listen on %s: %w", addr, err)
	}
	if !loopback(listener.Addr()) {
		log.Warn("profiling is reachable beyond this machine; name a loopback address unless that is meant",
			"addr", listener.Addr().String())
	}

	// The questions worth asking of a downloader are as often "what is it
	// waiting on" as "what is it computing", and Go records neither lock
	// contention nor blocking unless asked: without these the mutex and
	// block profiles are served empty. Sampled, so the cost is small, and
	// only while profiling is on.
	runtime.SetMutexProfileFraction(profileMutexFraction)
	runtime.SetBlockProfileRate(int(profileBlockRate))

	mux := http.NewServeMux()
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)

	// No write timeout: a CPU profile or a trace is a response that takes
	// as long as it was asked to.
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: config.ReadHeaderTimeout}
	go func() {
		if err := srv.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Warn("profiling stopped", "err", err)
		}
	}()
	log.Info("profiling", "url", "http://"+listener.Addr().String()+"/debug/pprof/")
	return func() { _ = srv.Close() }, nil
}

// One contention event in profileMutexFraction is recorded, and blocking is
// sampled at one event per profileBlockRate spent blocked: enough to show
// where a queue waits, at a cost that does not change what is measured.
const (
	profileMutexFraction = 5
	profileBlockRate     = time.Millisecond
)

// loopback reports whether a listening address can only be reached from
// this machine.
func loopback(addr net.Addr) bool {
	tcp, ok := addr.(*net.TCPAddr)
	return ok && tcp.IP.IsLoopback()
}
