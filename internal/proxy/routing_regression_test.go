package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestNamedSubresourceRoutesToTargetContainingObject(t *testing.T) {
	tests := []struct {
		name           string
		method         string
		objectPath     string
		subresourceURL string
		body           string
	}{
		{
			name:           "get deployment scale",
			method:         http.MethodGet,
			objectPath:     "/apis/apps/v1/namespaces/default/deployments/demo",
			subresourceURL: "/apis/apps/v1/namespaces/default/deployments/demo/scale",
		},
		{
			name:           "patch deployment scale",
			method:         http.MethodPatch,
			objectPath:     "/apis/apps/v1/namespaces/default/deployments/demo",
			subresourceURL: "/apis/apps/v1/namespaces/default/deployments/demo/scale",
			body:           `{"spec":{"replicas":2}}`,
		},
		{
			name:           "create pod eviction",
			method:         http.MethodPost,
			objectPath:     "/api/v1/namespaces/default/pods/demo",
			subresourceURL: "/api/v1/namespaces/default/pods/demo/eviction",
			body:           `{"apiVersion":"policy/v1","kind":"Eviction","metadata":{"name":"demo","namespace":"default"}}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			calls := &callRecorder{}
			targets, cleanup := testTargets(t, map[string]http.HandlerFunc{
				"one": func(w http.ResponseWriter, r *http.Request) {
					calls.add("one:" + r.Method + ":" + r.URL.Path)
					http.NotFound(w, r)
				},
				"two": func(w http.ResponseWriter, r *http.Request) {
					calls.add("two:" + r.Method + ":" + r.URL.Path)
					switch {
					case r.Method == http.MethodGet && r.URL.Path == tt.objectPath:
						_, _ = w.Write([]byte(`{"metadata":{"name":"demo"}}`))
					case r.Method == tt.method && r.URL.Path == tt.subresourceURL:
						_, _ = w.Write([]byte(`{"metadata":{"name":"demo"},"spec":{"replicas":2}}`))
					default:
						http.NotFound(w, r)
					}
				},
			})
			defer cleanup()

			p, err := newTestProxy(targets, targets[0])
			if err != nil {
				t.Fatal(err)
			}

			req := httptest.NewRequest(tt.method, tt.subresourceURL, strings.NewReader(tt.body))
			rec := httptest.NewRecorder()
			serveTestHTTP(p, rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
			}
			wantCalls := []string{
				"one:GET:" + tt.objectPath,
				"two:GET:" + tt.objectPath,
				"two:" + tt.method + ":" + tt.subresourceURL,
			}
			if got := calls.snapshot(); !slices.Equal(got, wantCalls) {
				t.Fatalf("calls = %v, want %v", got, wantCalls)
			}
		})
	}
}

func TestNamedFieldSelectorWatchWaitsForFutureObject(t *testing.T) {
	watchOpened := make(chan string, 2)
	releaseWatch := make(chan struct{})
	targets, cleanup := testTargets(t, map[string]http.HandlerFunc{
		"one": futureObjectWatchHandler("one", watchOpened, releaseWatch),
		"two": futureObjectWatchHandler("two", watchOpened, releaseWatch),
	})
	defer cleanup()

	p, err := newTestProxy(targets, targets[0])
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/namespaces/default/pods?watch=true&fieldSelector=metadata.name%3Ddemo", http.NoBody)
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		serveTestHTTP(p, rec, req)
	}()

	opened := make([]string, 0, len(targets))
	for range targets {
		select {
		case targetName := <-watchOpened:
			opened = append(opened, targetName)
		case <-done:
			t.Fatal("watch request returned before upstream watch streams opened")
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for upstream watch streams")
		}
	}
	close(releaseWatch)

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for aggregate watch to finish")
	}

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if !slices.Contains(opened, "one") || !slices.Contains(opened, "two") {
		t.Fatalf("opened streams = %v, want both targets", opened)
	}

	events := strings.Split(strings.TrimSpace(rec.Body.String()), "\n")
	if len(events) != 2 {
		t.Fatalf("events = %d, want 2; body=%s", len(events), rec.Body.String())
	}
	for _, line := range events {
		var event struct {
			Object struct {
				Metadata struct {
					Annotations map[string]string `json:"annotations"`
				} `json:"metadata"`
			} `json:"object"`
		}
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatal(err)
		}
		if event.Object.Metadata.Annotations[sourceContextAnnotation] == "" {
			t.Fatalf("event has no source context marker: %s", line)
		}
	}
}

func futureObjectWatchHandler(targetName string, opened chan<- string, release <-chan struct{}) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("watch") != "true" {
			_, _ = w.Write([]byte(`{"apiVersion":"v1","kind":"PodList","items":[]}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		opened <- targetName
		<-release
		_, _ = w.Write([]byte(`{"type":"ADDED","object":{"metadata":{"name":"demo"}}}` + "\n"))
	}
}
