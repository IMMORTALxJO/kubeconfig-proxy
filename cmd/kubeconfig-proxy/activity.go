package main

import (
	"crypto/subtle"
	"net/http"
	"strings"
	"sync/atomic"
	"time"
)

type activityHandler struct {
	next         http.Handler
	bearerToken  string
	lastActivity atomic.Int64
	inFlight     atomic.Int64
}

func newActivityHandler(next http.Handler, bearerToken string) *activityHandler {
	h := &activityHandler{next: next, bearerToken: bearerToken}
	h.lastActivity.Store(time.Now().UnixNano())
	return h
}

func (h *activityHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == readinessPath {
		if !authorizedWithToken(r, h.bearerToken) {
			writePlainStatus(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		h.lastActivity.Store(time.Now().UnixNano())
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
		return
	}

	h.inFlight.Add(1)
	h.lastActivity.Store(time.Now().UnixNano())
	defer func() {
		h.lastActivity.Store(time.Now().UnixNano())
		h.inFlight.Add(-1)
	}()
	h.next.ServeHTTP(w, r)
}

func (h *activityHandler) idleFor(ttl time.Duration) bool {
	if h.inFlight.Load() > 0 {
		return false
	}
	return time.Since(time.Unix(0, h.lastActivity.Load())) > ttl
}

func authorizedWithToken(r *http.Request, token string) bool {
	const prefix = "Bearer "
	header := r.Header.Get("Authorization")
	if !strings.HasPrefix(header, prefix) {
		return false
	}
	got := strings.TrimPrefix(header, prefix)
	return subtle.ConstantTimeCompare([]byte(got), []byte(token)) == 1
}

func writePlainStatus(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(message + "\n"))
}
