package main

import (
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/IMMORTALxJO/kubeconfig-proxy/internal/proxy"
)

type activityHandler struct {
	next         http.Handler
	bearerToken  string
	lastActivity atomic.Int64
	mu           sync.Mutex
	inFlight     int64
	isDraining   bool
	becameIdle   chan struct{}
}

func newActivityHandler(next http.Handler, bearerToken string) *activityHandler {
	h := &activityHandler{next: next, bearerToken: bearerToken, becameIdle: make(chan struct{}, 1)}
	h.lastActivity.Store(time.Now().UnixNano())
	return h
}

func (h *activityHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == readinessPath {
		if !proxy.AuthorizedWithToken(r, h.bearerToken) {
			writePlainStatus(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		if h.draining() {
			writePlainStatus(w, http.StatusServiceUnavailable, "reloading")
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
		return
	}

	if !h.beginRequest() {
		writePlainStatus(w, http.StatusServiceUnavailable, "reloading")
		return
	}
	h.lastActivity.Store(time.Now().UnixNano())
	defer func() {
		h.lastActivity.Store(time.Now().UnixNano())
		if h.finishRequest() {
			select {
			case h.becameIdle <- struct{}{}:
			default:
			}
		}
	}()
	h.next.ServeHTTP(w, r)
}

func (h *activityHandler) beginRequest() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.isDraining {
		return false
	}
	h.inFlight++
	return true
}

func (h *activityHandler) finishRequest() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.inFlight--
	return h.inFlight == 0
}

func (h *activityHandler) beginDrainIfIdle() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.inFlight != 0 || h.isDraining {
		return false
	}
	h.isDraining = true
	return true
}

func (h *activityHandler) draining() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.isDraining
}

func (h *activityHandler) isIdleFor(ttl time.Duration) bool {
	h.mu.Lock()
	hasInFlightRequests := h.inFlight > 0
	h.mu.Unlock()
	if hasInFlightRequests {
		return false
	}
	return time.Since(time.Unix(0, h.lastActivity.Load())) > ttl
}

func writePlainStatus(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(message + "\n"))
}
