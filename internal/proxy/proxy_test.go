package proxy

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type seenUpdate struct {
	target          string
	uid             string
	resourceVersion string
}

const testBearerToken = "test-token"

func TestDefaultRetries(t *testing.T) {
	if DefaultRetries != 5 {
		t.Fatalf("DefaultRetries = %d, want 5", DefaultRetries)
	}
}

func TestNewWithOptionsValidatesInputs(t *testing.T) {
	target := Target{Name: "one", Host: mustParseURL(t, "https://one.example.test"), Client: http.DefaultClient}
	tests := []struct {
		name            string
		targets         []Target
		options         Options
		wantErrContains string
	}{
		{
			name:            "empty targets",
			options:         Options{BearerToken: testBearerToken},
			wantErrContains: "at least one target is required",
		},
		{
			name:            "negative retries",
			targets:         []Target{target},
			options:         Options{BearerToken: testBearerToken, Retries: -1},
			wantErrContains: "retries must be greater than or equal to 0",
		},
		{
			name:            "negative request timeout",
			targets:         []Target{target},
			options:         Options{BearerToken: testBearerToken, RequestTimeout: -time.Second},
			wantErrContains: "request timeout must be greater than or equal to 0",
		},
		{
			name:            "negative retry backoff",
			targets:         []Target{target},
			options:         Options{BearerToken: testBearerToken, RetryBackoff: -time.Millisecond},
			wantErrContains: "retry backoff must be greater than or equal to 0",
		},
		{
			name:            "missing bearer token",
			targets:         []Target{target},
			options:         Options{},
			wantErrContains: "bearer token is required",
		},
		{
			name:            "duplicate target",
			targets:         []Target{target, target},
			options:         Options{BearerToken: testBearerToken},
			wantErrContains: `target "one" is configured more than once`,
		},
		{
			name:            "primary target is not configured",
			targets:         []Target{target},
			options:         Options{BearerToken: testBearerToken},
			wantErrContains: `primary target "missing" is not configured`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			primary := target
			if tt.name == "primary target is not configured" {
				primary.Name = "missing"
			}
			_, err := NewWithOptions(tt.targets, primary, tt.options)
			if err == nil {
				t.Fatal("NewWithOptions returned nil error")
			}
			if !strings.Contains(err.Error(), tt.wantErrContains) {
				t.Fatalf("error = %q, want to contain %q", err.Error(), tt.wantErrContains)
			}
		})
	}
}

type callRecorder struct {
	mu    sync.Mutex
	calls []string
}

func (r *callRecorder) add(call string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, call)
}

func (r *callRecorder) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.calls...)
}

func TestFanOutMutationsToAllTargets(t *testing.T) {
	calls := &callRecorder{}
	targets, cleanup := testTargets(t, map[string]http.HandlerFunc{
		"one": func(w http.ResponseWriter, r *http.Request) {
			calls.add("one:" + r.URL.Path)
			_, _ = io.Copy(io.Discard, r.Body)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"kind":"Deployment","metadata":{"name":"demo"}}`))
		},
		"two": func(w http.ResponseWriter, r *http.Request) {
			calls.add("two:" + r.URL.Path)
			_, _ = io.Copy(io.Discard, r.Body)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"kind":"Deployment","metadata":{"name":"demo"}}`))
		},
	})
	defer cleanup()

	p, err := newTestProxy(targets, targets[0])
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost, "/apis/apps/v1/namespaces/default/deployments", http.NoBody)
	rec := httptest.NewRecorder()
	serveTestHTTP(p, rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	gotCalls := calls.snapshot()
	if len(gotCalls) != 2 {
		t.Fatalf("calls = %v, want two upstream calls", gotCalls)
	}
}

func TestJobMutationsFanOutWithoutAnnotations(t *testing.T) {
	calls := &callRecorder{}
	targets, cleanup := testTargets(t, map[string]http.HandlerFunc{
		"one": func(w http.ResponseWriter, _ *http.Request) {
			calls.add("one")
			_, _ = w.Write([]byte(`{"kind":"Job","metadata":{"name":"demo"}}`))
		},
		"two": func(w http.ResponseWriter, _ *http.Request) {
			calls.add("two")
			_, _ = w.Write([]byte(`{"kind":"Job","metadata":{"name":"demo"}}`))
		},
	})
	defer cleanup()

	p, err := newTestProxy(targets, targets[0])
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost, "/apis/batch/v1/namespaces/default/jobs", http.NoBody)
	rec := httptest.NewRecorder()
	serveTestHTTP(p, rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	gotCalls := calls.snapshot()
	if len(gotCalls) != 2 {
		t.Fatalf("calls = %v, want two upstream calls", gotCalls)
	}
}

func TestPutFanOutRewritesObjectIdentityPerTarget(t *testing.T) {
	var (
		mu      sync.Mutex
		updates []seenUpdate
	)
	targets, cleanup := testTargets(t, map[string]http.HandlerFunc{
		"one": func(w http.ResponseWriter, r *http.Request) {
			switch r.Method {
			case http.MethodGet:
				_, _ = w.Write([]byte(`{"metadata":{"name":"demo","uid":"uid-one","resourceVersion":"10"}}`))
			case http.MethodPut:
				update := decodeUpdate(t, "one", r)
				mu.Lock()
				updates = append(updates, update)
				mu.Unlock()
				_, _ = w.Write([]byte(`{"metadata":{"name":"demo"}}`))
			default:
				t.Fatalf("unexpected method %s", r.Method)
			}
		},
		"two": func(w http.ResponseWriter, r *http.Request) {
			switch r.Method {
			case http.MethodGet:
				_, _ = w.Write([]byte(`{"metadata":{"name":"demo","uid":"uid-two","resourceVersion":"20"}}`))
			case http.MethodPut:
				update := decodeUpdate(t, "two", r)
				mu.Lock()
				updates = append(updates, update)
				mu.Unlock()
				_, _ = w.Write([]byte(`{"metadata":{"name":"demo"}}`))
			default:
				t.Fatalf("unexpected method %s", r.Method)
			}
		},
	})
	defer cleanup()

	p, err := newTestProxy(targets, targets[0])
	if err != nil {
		t.Fatal(err)
	}

	body := strings.NewReader(`{"metadata":{"name":"demo","uid":"primary-uid","resourceVersion":"1"}}`)
	req := httptest.NewRequest(http.MethodPut, "/api/v1/namespaces/default/configmaps/demo", body)
	rec := httptest.NewRecorder()
	serveTestHTTP(p, rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}

	mu.Lock()
	defer mu.Unlock()
	if len(updates) != 2 {
		t.Fatalf("updates = %#v, want two updates", updates)
	}

	byTarget := map[string]seenUpdate{}
	for _, update := range updates {
		byTarget[update.target] = update
	}
	if got := byTarget["one"]; got.uid != "uid-one" || got.resourceVersion != "10" {
		t.Fatalf("one update = %#v", got)
	}
	if got := byTarget["two"]; got.uid != "uid-two" || got.resourceVersion != "20" {
		t.Fatalf("two update = %#v", got)
	}
}

func TestContextNameAnnotationRoutesMutationToNamedTarget(t *testing.T) {
	calls := &callRecorder{}
	targets, cleanup := testTargets(t, map[string]http.HandlerFunc{
		"one": func(w http.ResponseWriter, _ *http.Request) {
			calls.add("one")
			_, _ = w.Write([]byte(`{"kind":"ConfigMap","metadata":{"name":"demo"}}`))
		},
		"two": func(w http.ResponseWriter, _ *http.Request) {
			calls.add("two")
			_, _ = w.Write([]byte(`{"kind":"ConfigMap","metadata":{"name":"demo"}}`))
		},
	})
	defer cleanup()

	p, err := newTestProxy(targets, targets[0])
	if err != nil {
		t.Fatal(err)
	}

	body := strings.NewReader(`{
		"kind":"ConfigMap",
		"metadata":{
			"name":"demo",
			"annotations":{"kubeconfig-proxy.io/context-name":"two"}
		}
	}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/namespaces/default/configmaps", body)
	rec := httptest.NewRecorder()
	serveTestHTTP(p, rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	gotCalls := calls.snapshot()
	if len(gotCalls) != 1 || gotCalls[0] != "two" {
		t.Fatalf("calls = %v, want target from annotation", gotCalls)
	}
}

func TestSingleContextAnnotationRoutesMutationToAlphabeticallyFirstTarget(t *testing.T) {
	calls := &callRecorder{}
	targets, cleanup := testTargets(t, map[string]http.HandlerFunc{
		"one": func(w http.ResponseWriter, _ *http.Request) {
			calls.add("one")
			_, _ = w.Write([]byte(`{"kind":"ConfigMap","metadata":{"name":"demo"}}`))
		},
		"two": func(w http.ResponseWriter, _ *http.Request) {
			calls.add("two")
			_, _ = w.Write([]byte(`{"kind":"ConfigMap","metadata":{"name":"demo"}}`))
		},
	})
	defer cleanup()

	p, err := newTestProxy(targets, targets[1])
	if err != nil {
		t.Fatal(err)
	}

	body := strings.NewReader(`{
		"kind":"ConfigMap",
		"metadata":{
			"name":"demo",
			"annotations":{"kubeconfig-proxy.io/single-context":"true"}
		}
	}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/namespaces/default/configmaps", body)
	rec := httptest.NewRecorder()
	serveTestHTTP(p, rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	gotCalls := calls.snapshot()
	if len(gotCalls) != 1 || gotCalls[0] != "one" {
		t.Fatalf("calls = %v, want alphabetically first target", gotCalls)
	}
}

func TestSingleContextAnnotationRoutesYAMLMutationBody(t *testing.T) {
	calls := &callRecorder{}
	targets, cleanup := testTargets(t, map[string]http.HandlerFunc{
		"one": func(w http.ResponseWriter, _ *http.Request) {
			calls.add("one")
			_, _ = w.Write([]byte(`{"kind":"ConfigMap","metadata":{"name":"demo"}}`))
		},
		"two": func(w http.ResponseWriter, _ *http.Request) {
			calls.add("two")
			_, _ = w.Write([]byte(`{"kind":"ConfigMap","metadata":{"name":"demo"}}`))
		},
	})
	defer cleanup()

	p, err := newTestProxy(targets, targets[0])
	if err != nil {
		t.Fatal(err)
	}

	body := strings.NewReader(`
kind: ConfigMap
metadata:
  name: demo
  annotations:
    kubeconfig-proxy.io/single-context: "true"
`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/namespaces/default/configmaps", body)
	rec := httptest.NewRecorder()
	serveTestHTTP(p, rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	gotCalls := calls.snapshot()
	if len(gotCalls) != 1 || gotCalls[0] != "one" {
		t.Fatalf("calls = %v, want alphabetically first target", gotCalls)
	}
}

func TestDeleteNamedResourceUsesExistingResourceAnnotations(t *testing.T) {
	calls := &callRecorder{}
	targets, cleanup := testTargets(t, map[string]http.HandlerFunc{
		"one": func(w http.ResponseWriter, r *http.Request) {
			switch r.Method {
			case http.MethodGet:
				calls.add("one:get")
				_, _ = w.Write([]byte(`{"kind":"Job","metadata":{"name":"demo","annotations":{"kubeconfig-proxy.io/single-context":"true"}}}`))
			case http.MethodDelete:
				calls.add("one:delete")
				_, _ = w.Write([]byte(`{"kind":"Status","status":"Success"}`))
			default:
				t.Fatalf("unexpected method %s", r.Method)
			}
		},
		"two": func(w http.ResponseWriter, r *http.Request) {
			switch r.Method {
			case http.MethodGet:
				calls.add("two:get")
				http.NotFound(w, r)
			case http.MethodDelete:
				t.Fatalf("delete should be routed only to the annotated target")
			default:
				t.Fatalf("unexpected method %s", r.Method)
			}
		},
	})
	defer cleanup()

	p, err := newTestProxy(targets, targets[0])
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodDelete, "/apis/batch/v1/namespaces/default/jobs/demo", http.NoBody)
	rec := httptest.NewRecorder()
	serveTestHTTP(p, rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	gotCalls := calls.snapshot()
	if !slices.Contains(gotCalls, "one:get") || !slices.Contains(gotCalls, "one:delete") {
		t.Fatalf("calls = %v, want get and delete on annotated target", gotCalls)
	}
}

func TestDeleteNamedResourceRoutesOnlyToTargetsContainingObject(t *testing.T) {
	calls := &callRecorder{}
	targets, cleanup := testTargets(t, map[string]http.HandlerFunc{
		"one": func(w http.ResponseWriter, r *http.Request) {
			switch r.Method {
			case http.MethodGet:
				calls.add("one:get")
				_, _ = w.Write([]byte(`{"kind":"Pod","metadata":{"name":"demo"}}`))
			case http.MethodDelete:
				calls.add("one:delete")
				_, _ = w.Write([]byte(`{"kind":"Status","status":"Success"}`))
			default:
				t.Fatalf("unexpected method %s", r.Method)
			}
		},
		"two": func(w http.ResponseWriter, r *http.Request) {
			switch r.Method {
			case http.MethodGet:
				calls.add("two:get")
				http.NotFound(w, r)
			case http.MethodDelete:
				t.Fatalf("delete should not be routed to a target where the object is missing")
			default:
				t.Fatalf("unexpected method %s", r.Method)
			}
		},
	})
	defer cleanup()

	p, err := newTestProxy(targets, targets[0])
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/namespaces/default/pods/demo", http.NoBody)
	rec := httptest.NewRecorder()
	serveTestHTTP(p, rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	gotCalls := calls.snapshot()
	for _, want := range []string{"one:get", "two:get", "one:delete"} {
		if !slices.Contains(gotCalls, want) {
			t.Fatalf("calls = %v, want %s", gotCalls, want)
		}
	}
}

func TestPatchNamedResourceUsesExistingResourceAnnotations(t *testing.T) {
	calls := &callRecorder{}
	targets, cleanup := testTargets(t, map[string]http.HandlerFunc{
		"one": func(w http.ResponseWriter, r *http.Request) {
			switch r.Method {
			case http.MethodGet:
				calls.add("one:get")
				http.NotFound(w, r)
			case http.MethodPatch:
				t.Fatalf("patch should be routed only to the annotated target")
			default:
				t.Fatalf("unexpected method %s", r.Method)
			}
		},
		"two": func(w http.ResponseWriter, r *http.Request) {
			switch r.Method {
			case http.MethodGet:
				calls.add("two:get")
				_, _ = w.Write([]byte(`{"kind":"ConfigMap","metadata":{"name":"demo","annotations":{"kubeconfig-proxy.io/context-name":"two"}}}`))
			case http.MethodPatch:
				calls.add("two:patch")
				_, _ = io.Copy(io.Discard, r.Body)
				_, _ = w.Write([]byte(`{"kind":"ConfigMap","metadata":{"name":"demo"}}`))
			default:
				t.Fatalf("unexpected method %s", r.Method)
			}
		},
	})
	defer cleanup()

	p, err := newTestProxy(targets, targets[0])
	if err != nil {
		t.Fatal(err)
	}

	body := strings.NewReader(`{"data":{"key":"value"}}`)
	req := httptest.NewRequest(http.MethodPatch, "/api/v1/namespaces/default/configmaps/demo", body)
	req.Header.Set("Content-Type", "application/merge-patch+json")
	rec := httptest.NewRecorder()
	serveTestHTTP(p, rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	gotCalls := calls.snapshot()
	if !slices.Contains(gotCalls, "one:get") || !slices.Contains(gotCalls, "two:get") || !slices.Contains(gotCalls, "two:patch") {
		t.Fatalf("calls = %v, want lookup on targets and patch on annotated target", gotCalls)
	}
}

const (
	testPodPath                    = "/api/v1/namespaces/default/pods/demo"
	testPodEphemeralContainersPath = testPodPath + "/ephemeralcontainers"
)

func TestPutPodEphemeralContainersUsesExistingPodTarget(t *testing.T) {
	calls := &callRecorder{}
	targets, cleanup := testTargets(t, map[string]http.HandlerFunc{
		"one": missingPodMutationHandler(t, calls),
		"two": existingPodMutationHandler(t, calls),
	})
	defer cleanup()

	p, err := newTestProxy(targets, targets[0])
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPut, testPodEphemeralContainersPath, strings.NewReader(`{"ephemeralContainers":[]}`))
	rec := httptest.NewRecorder()
	serveTestHTTP(p, rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	gotCalls := calls.snapshot()
	for _, want := range []string{"one:get", "two:get", "two:get-ephemeralcontainers", "two:put"} {
		if !slices.Contains(gotCalls, want) {
			t.Fatalf("calls = %v, want %s", gotCalls, want)
		}
	}
}

func missingPodMutationHandler(t *testing.T, calls *callRecorder) http.HandlerFunc {
	t.Helper()

	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			expectTestRequestPath(t, r, testPodPath)
			calls.add("one:get")
			http.NotFound(w, r)
			return
		}
		if r.Method == http.MethodPut {
			t.Fatal("put should not be routed to a target where the pod is missing")
		}
		t.Fatalf("unexpected method %s", r.Method)
	}
}

func existingPodMutationHandler(t *testing.T, calls *callRecorder) http.HandlerFunc {
	t.Helper()

	return func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			handleExistingPodLookup(t, calls, w, r)
		case http.MethodPut:
			expectTestRequestPath(t, r, testPodEphemeralContainersPath)
			calls.add("two:put")
			_, _ = io.Copy(io.Discard, r.Body)
			writeTestPod(w)
		default:
			t.Fatalf("unexpected method %s", r.Method)
		}
	}
}

func handleExistingPodLookup(t *testing.T, calls *callRecorder, w http.ResponseWriter, r *http.Request) {
	t.Helper()

	switch r.URL.Path {
	case testPodPath:
		calls.add("two:get")
		writeTestPod(w)
	case testPodEphemeralContainersPath:
		calls.add("two:get-ephemeralcontainers")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	default:
		t.Fatalf("lookup path = %s, want pod or ephemeralcontainers path", r.URL.Path)
	}
}

func expectTestRequestPath(t *testing.T, r *http.Request, want string) {
	t.Helper()

	if r.URL.Path != want {
		t.Fatalf("request path = %s, want %s", r.URL.Path, want)
	}
}

func writeTestPod(w http.ResponseWriter) {
	_, _ = w.Write([]byte(`{"kind":"Pod","metadata":{"name":"demo"}}`))
}

func TestContextNameAnnotationRejectsUnknownTarget(t *testing.T) {
	targets, cleanup := testTargets(t, map[string]http.HandlerFunc{
		"one": func(_ http.ResponseWriter, _ *http.Request) {
			t.Fatalf("upstream should not be called")
		},
		"two": func(_ http.ResponseWriter, _ *http.Request) {
			t.Fatalf("upstream should not be called")
		},
	})
	defer cleanup()

	p, err := newTestProxy(targets, targets[0])
	if err != nil {
		t.Fatal(err)
	}

	body := strings.NewReader(`{
		"kind":"ConfigMap",
		"metadata":{
			"name":"demo",
			"annotations":{"kubeconfig-proxy.io/context-name":"missing"}
		}
	}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/namespaces/default/configmaps", body)
	rec := httptest.NewRecorder()
	serveTestHTTP(p, rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusBadRequest, rec.Body.String())
	}
}

func TestAggregatesListResponses(t *testing.T) {
	targets, cleanup := testTargets(t, map[string]http.HandlerFunc{
		"one": func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"apiVersion":"v1","kind":"PodList","metadata":{"resourceVersion":"10"},"items":[{"metadata":{"name":"a"}}]}`))
		},
		"two": func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"apiVersion":"v1","kind":"PodList","metadata":{"resourceVersion":"11"},"items":[{"metadata":{"name":"b"}}]}`))
		},
	})
	defer cleanup()

	p, err := newTestProxy(targets, targets[0])
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/namespaces/default/pods", http.NoBody)
	rec := httptest.NewRecorder()
	serveTestHTTP(p, rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}

	var payload struct {
		Items []struct {
			Metadata struct {
				Name        string            `json:"name"`
				Annotations map[string]string `json:"annotations"`
				Labels      map[string]string `json:"labels"`
			} `json:"metadata"`
		} `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Items) != 2 {
		t.Fatalf("items = %d, want 2", len(payload.Items))
	}
	if payload.Items[0].Metadata.Annotations["kubeconfig-proxy.io/context"] != "one" {
		t.Fatalf("first item annotations = %#v", payload.Items[0].Metadata.Annotations)
	}
	if payload.Items[0].Metadata.Labels["context"] != "one" {
		t.Fatalf("first item labels = %#v", payload.Items[0].Metadata.Labels)
	}
	if payload.Items[1].Metadata.Annotations["kubeconfig-proxy.io/context"] != "two" {
		t.Fatalf("second item annotations = %#v", payload.Items[1].Metadata.Annotations)
	}
	if payload.Items[1].Metadata.Labels["context"] != "two" {
		t.Fatalf("second item labels = %#v", payload.Items[1].Metadata.Labels)
	}
}

func TestAggregatedListsRequestJSONInsteadOfProtobuf(t *testing.T) {
	var acceptsMu sync.Mutex
	var accepts []string
	handler := func(w http.ResponseWriter, r *http.Request) {
		acceptsMu.Lock()
		accepts = append(accepts, r.Header.Get("Accept"))
		acceptsMu.Unlock()
		_, _ = w.Write([]byte(`{"apiVersion":"v1","kind":"PodList","metadata":{"resourceVersion":"10"},"items":[]}`))
	}
	targets, cleanup := testTargets(t, map[string]http.HandlerFunc{
		"one": handler,
		"two": handler,
	})
	defer cleanup()

	p, err := newTestProxy(targets, targets[0])
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/namespaces/default/pods", http.NoBody)
	req.Header.Set("Accept", "application/json;as=Table;g=meta.k8s.io;v=v1, application/vnd.kubernetes.protobuf;as=Table;g=meta.k8s.io;v=v1, application/json, application/vnd.kubernetes.protobuf")
	rec := httptest.NewRecorder()
	serveTestHTTP(p, rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if len(accepts) != 2 {
		t.Fatalf("upstream requests = %d, want 2", len(accepts))
	}
	for _, accept := range accepts {
		if strings.Contains(accept, "application/vnd.kubernetes.protobuf") {
			t.Fatalf("upstream Accept = %q, must not include protobuf", accept)
		}
		if !strings.Contains(accept, "application/json;as=Table;g=meta.k8s.io;v=v1") {
			t.Fatalf("upstream Accept = %q, must preserve JSON Table media range", accept)
		}
	}
}

func TestAggregatesGzipListResponses(t *testing.T) {
	targets, cleanup := testTargets(t, map[string]http.HandlerFunc{
		"one": gzipListHandler(t, `{"apiVersion":"v1","kind":"PodList","metadata":{"resourceVersion":"10"},"items":[{"metadata":{"name":"a"}}]}`),
		"two": gzipListHandler(t, `{"apiVersion":"v1","kind":"PodList","metadata":{"resourceVersion":"11"},"items":[{"metadata":{"name":"b"}}]}`),
	})
	defer cleanup()

	p, err := newTestProxy(targets, targets[0])
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/namespaces/default/pods", http.NoBody)
	req.Header.Set("Accept-Encoding", "gzip")
	rec := httptest.NewRecorder()
	serveTestHTTP(p, rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}

	var payload struct {
		Items []struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
		} `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Items) != 2 {
		t.Fatalf("items = %d, want 2", len(payload.Items))
	}
}

func TestAggregatesPaginatedListAcrossTargets(t *testing.T) {
	targets, cleanup := testTargets(t, map[string]http.HandlerFunc{
		"one": paginatedListHandler(t, "10", []string{"a1", "a2", "a3"}),
		"two": paginatedListHandler(t, "20", []string{"b1", "b2"}),
	})
	defer cleanup()

	p, err := newTestProxy(targets, targets[0])
	if err != nil {
		t.Fatal(err)
	}

	result := collectPaginatedList(t, p, 2)
	if result.pageCount != 3 {
		t.Fatalf("page count = %d, want 3", result.pageCount)
	}
	if want := []string{"a1", "a2", "a3", "b1", "b2"}; !slices.Equal(result.names, want) {
		t.Fatalf("names = %v, want %v", result.names, want)
	}
	if want := []string{"one", "one", "one", "two", "two"}; !slices.Equal(result.contexts, want) {
		t.Fatalf("contexts = %v, want %v", result.contexts, want)
	}
	resourceVersions, ok := decodeAggregateResourceVersion(result.resourceVersion)
	if !ok {
		t.Fatalf("final resourceVersion = %q, want aggregate resource version", result.resourceVersion)
	}
	if want := map[string]string{"one": "10", "two": "20"}; !mapsEqual(resourceVersions, want) {
		t.Fatalf("resourceVersions = %v, want %v", resourceVersions, want)
	}
}

type paginatedListResult struct {
	names           []string
	contexts        []string
	resourceVersion string
	pageCount       int
}

func collectPaginatedList(t *testing.T, p *Proxy, limit int) paginatedListResult {
	t.Helper()

	var result paginatedListResult
	var names []string
	var contexts []string
	continueToken := ""
	for {
		result.pageCount++
		if result.pageCount > 4 {
			t.Fatal("pagination did not terminate")
		}
		query := url.Values{"limit": []string{strconv.Itoa(limit)}}
		if continueToken != "" {
			query.Set("continue", continueToken)
		}
		req := httptest.NewRequest(http.MethodGet, "/api/v1/namespaces/default/pods?"+query.Encode(), http.NoBody)
		rec := httptest.NewRecorder()
		serveTestHTTP(p, rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("page %d status = %d, want %d; body=%s", result.pageCount, rec.Code, http.StatusOK, rec.Body.String())
		}

		var payload struct {
			Metadata struct {
				Continue        string `json:"continue"`
				ResourceVersion string `json:"resourceVersion"`
			} `json:"metadata"`
			Items []struct {
				Metadata struct {
					Name   string            `json:"name"`
					Labels map[string]string `json:"labels"`
				} `json:"metadata"`
			} `json:"items"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
			t.Fatal(err)
		}
		if len(payload.Items) > limit {
			t.Fatalf("page %d returned %d items for global limit %d", result.pageCount, len(payload.Items), limit)
		}
		for _, item := range payload.Items {
			names = append(names, item.Metadata.Name)
			contexts = append(contexts, item.Metadata.Labels[sourceContextLabel])
		}
		continueToken = payload.Metadata.Continue
		result.resourceVersion = payload.Metadata.ResourceVersion
		if continueToken == "" {
			break
		}
		if !strings.HasPrefix(continueToken, aggregateContinuePrefix) {
			t.Fatalf("page %d continue token = %q, want aggregate token", result.pageCount, continueToken)
		}
	}
	result.names = names
	result.contexts = contexts
	return result
}

func TestAggregatedListRejectsInvalidContinueTokens(t *testing.T) {
	targets, cleanup := testTargets(t, map[string]http.HandlerFunc{
		"one": paginatedListHandler(t, "10", []string{"a1", "a2"}),
	})
	defer cleanup()
	p, err := newTestProxy(targets, targets[0])
	if err != nil {
		t.Fatal(err)
	}

	mismatchedToken := encodeListCursor([]string{"other"}, "other", "", nil, "/api/v1/pods?")
	for _, continueToken := range []string{"plain-upstream-token", mismatchedToken} {
		query := url.Values{"limit": []string{"1"}, "continue": []string{continueToken}}
		req := httptest.NewRequest(http.MethodGet, "/api/v1/pods?"+query.Encode(), http.NoBody)
		rec := httptest.NewRecorder()
		serveTestHTTP(p, rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("continue %q status = %d, want %d; body=%s", continueToken, rec.Code, http.StatusBadRequest, rec.Body.String())
		}
	}

	firstQuery := url.Values{"limit": []string{"1"}, "labelSelector": []string{"app=one"}}
	firstReq := httptest.NewRequest(http.MethodGet, "/api/v1/pods?"+firstQuery.Encode(), http.NoBody)
	firstRec := httptest.NewRecorder()
	serveTestHTTP(p, firstRec, firstReq)
	if firstRec.Code != http.StatusOK {
		t.Fatalf("first page status = %d, want %d; body=%s", firstRec.Code, http.StatusOK, firstRec.Body.String())
	}
	var firstPage struct {
		Metadata struct {
			Continue string `json:"continue"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal(firstRec.Body.Bytes(), &firstPage); err != nil {
		t.Fatal(err)
	}
	if firstPage.Metadata.Continue == "" {
		t.Fatal("first page continue token is empty")
	}
	changedQuery := url.Values{
		"limit":         []string{"1"},
		"labelSelector": []string{"app=two"},
		"continue":      []string{firstPage.Metadata.Continue},
	}
	changedReq := httptest.NewRequest(http.MethodGet, "/api/v1/pods?"+changedQuery.Encode(), http.NoBody)
	changedRec := httptest.NewRecorder()
	serveTestHTTP(p, changedRec, changedReq)
	if changedRec.Code != http.StatusBadRequest {
		t.Fatalf("changed selector status = %d, want %d; body=%s", changedRec.Code, http.StatusBadRequest, changedRec.Body.String())
	}
}

func TestAggregateWatchUsesPerTargetResourceVersions(t *testing.T) {
	calls := &callRecorder{}
	targets, cleanup := testTargets(t, map[string]http.HandlerFunc{
		"one": func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Query().Get("watch") == "true" {
				calls.add("one:" + r.URL.Query().Get("resourceVersion"))
				_, _ = w.Write([]byte(`{"type":"BOOKMARK","object":{"metadata":{"name":"a"}}}` + "\n"))
				return
			}
			_, _ = w.Write([]byte(`{"apiVersion":"v1","kind":"PodList","metadata":{"resourceVersion":"10"},"items":[{"metadata":{"name":"a"}}]}`))
		},
		"two": func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Query().Get("watch") == "true" {
				calls.add("two:" + r.URL.Query().Get("resourceVersion"))
				_, _ = w.Write([]byte(`{"type":"BOOKMARK","object":{"metadata":{"name":"b"}}}` + "\n"))
				return
			}
			_, _ = w.Write([]byte(`{"apiVersion":"v1","kind":"PodList","metadata":{"resourceVersion":"11"},"items":[{"metadata":{"name":"b"}}]}`))
		},
	})
	defer cleanup()

	p, err := newTestProxy(targets, targets[0])
	if err != nil {
		t.Fatal(err)
	}

	listReq := httptest.NewRequest(http.MethodGet, "/api/v1/namespaces/default/pods", http.NoBody)
	listRec := httptest.NewRecorder()
	serveTestHTTP(p, listRec, listReq)
	if listRec.Code != http.StatusOK {
		t.Fatalf("list status = %d, want %d; body=%s", listRec.Code, http.StatusOK, listRec.Body.String())
	}

	var listPayload struct {
		Metadata struct {
			ResourceVersion string `json:"resourceVersion"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal(listRec.Body.Bytes(), &listPayload); err != nil {
		t.Fatal(err)
	}
	if listPayload.Metadata.ResourceVersion == "" {
		t.Fatalf("list resourceVersion is empty; body=%s", listRec.Body.String())
	}

	query := url.Values{}
	query.Set("watch", "true")
	query.Set("resourceVersion", listPayload.Metadata.ResourceVersion)
	watchReq := httptest.NewRequest(http.MethodGet, "/api/v1/namespaces/default/pods?"+query.Encode(), http.NoBody)
	watchRec := httptest.NewRecorder()
	serveTestHTTP(p, watchRec, watchReq)

	if watchRec.Code != http.StatusOK {
		t.Fatalf("watch status = %d, want %d; body=%s", watchRec.Code, http.StatusOK, watchRec.Body.String())
	}
	gotCalls := calls.snapshot()
	for _, want := range []string{"one:10", "two:11"} {
		if !slices.Contains(gotCalls, want) {
			t.Fatalf("calls = %v, want %s", gotCalls, want)
		}
	}
}

func TestAggregateWatchForMissingNamedResourceClosesImmediately(t *testing.T) {
	calls := &callRecorder{}
	targets, cleanup := testTargets(t, map[string]http.HandlerFunc{
		"one": func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Query().Get("watch") == "true" {
				t.Fatalf("watch should not be opened when selected list is empty")
			}
			calls.add("one:list")
			_, _ = w.Write([]byte(`{"apiVersion":"v1","kind":"PodList","metadata":{"resourceVersion":"10"},"items":[]}`))
		},
		"two": func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Query().Get("watch") == "true" {
				t.Fatalf("watch should not be opened when selected list is empty")
			}
			calls.add("two:list")
			_, _ = w.Write([]byte(`{"apiVersion":"v1","kind":"PodList","metadata":{"resourceVersion":"11"},"items":[]}`))
		},
	})
	defer cleanup()

	p, err := newTestProxy(targets, targets[0])
	if err != nil {
		t.Fatal(err)
	}

	query := url.Values{}
	query.Set("watch", "true")
	query.Set("fieldSelector", "metadata.name=demo")
	req := httptest.NewRequest(http.MethodGet, "/api/v1/namespaces/default/pods?"+query.Encode(), http.NoBody)
	rec := httptest.NewRecorder()
	serveTestHTTP(p, rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("body = %q, want empty closed watch response", rec.Body.String())
	}
	gotCalls := calls.snapshot()
	for _, want := range []string{"one:list", "two:list"} {
		if !slices.Contains(gotCalls, want) {
			t.Fatalf("calls = %v, want %s", gotCalls, want)
		}
	}
}

func TestAggregateWatchPropagatesOpenFailure(t *testing.T) {
	targets, cleanup := testTargets(t, map[string]http.HandlerFunc{
		"one": func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "watch rejected", http.StatusForbidden)
		},
		"two": func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"type":"BOOKMARK"}` + "\n"))
		},
	})
	defer cleanup()

	p, err := newTestProxy(targets, targets[0])
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/pods?watch=true", http.NoBody)
	rec := httptest.NewRecorder()
	serveTestHTTP(p, rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusForbidden, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "watch rejected") {
		t.Fatalf("body = %q, want upstream watch rejection", rec.Body.String())
	}
}

func TestAggregatesTableResponses(t *testing.T) {
	targets, cleanup := testTargets(t, map[string]http.HandlerFunc{
		"one": func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{
				"apiVersion":"meta.k8s.io/v1",
				"kind":"Table",
				"metadata":{"resourceVersion":"10"},
				"columnDefinitions":[{"name":"Name","type":"string","format":"","description":"name"}],
				"rows":[{"cells":["a"],"object":{"metadata":{"name":"a"}}}]
			}`))
		},
		"two": func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{
				"apiVersion":"meta.k8s.io/v1",
				"kind":"Table",
				"metadata":{"resourceVersion":"11"},
				"columnDefinitions":[{"name":"Name","type":"string","format":"","description":"name"}],
				"rows":[{"cells":["b"],"object":{"metadata":{"name":"b"}}}]
			}`))
		},
	})
	defer cleanup()

	p, err := newTestProxy(targets, targets[0])
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/namespaces/default/configmaps", http.NoBody)
	rec := httptest.NewRecorder()
	serveTestHTTP(p, rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}

	var payload struct {
		ColumnDefinitions []struct {
			Name string `json:"name"`
		} `json:"columnDefinitions"`
		Rows []struct {
			Cells  []string `json:"cells"`
			Object struct {
				Metadata struct {
					Labels map[string]string `json:"labels"`
				} `json:"metadata"`
			} `json:"object"`
		} `json:"rows"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.ColumnDefinitions) != 1 || payload.ColumnDefinitions[0].Name != "Name" {
		t.Fatalf("columns = %#v, want original upstream columns", payload.ColumnDefinitions)
	}
	if len(payload.Rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(payload.Rows))
	}
	if got := payload.Rows[0].Cells; len(got) != 1 || got[0] != "a" {
		t.Fatalf("first row cells = %#v", got)
	}
	if payload.Rows[0].Object.Metadata.Labels["context"] != "one" {
		t.Fatalf("first row object labels = %#v", payload.Rows[0].Object.Metadata.Labels)
	}
	if got := payload.Rows[1].Cells; len(got) != 1 || got[0] != "b" {
		t.Fatalf("second row cells = %#v", got)
	}
	if payload.Rows[1].Object.Metadata.Labels["context"] != "two" {
		t.Fatalf("second row object labels = %#v", payload.Rows[1].Object.Metadata.Labels)
	}
}

func TestAggregateListPropagatesUpstreamError(t *testing.T) {
	targets, cleanup := testTargets(t, map[string]http.HandlerFunc{
		"one": func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "temporary outage", http.StatusServiceUnavailable)
		},
		"two": func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"items":[]}`))
		},
	})
	defer cleanup()

	p, err := newTestProxy(targets, targets[0])
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/configmaps", http.NoBody)
	rec := httptest.NewRecorder()
	serveTestHTTP(p, rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusServiceUnavailable, rec.Body.String())
	}
}

func TestAggregateListRejectsInvalidJSON(t *testing.T) {
	targets, cleanup := testTargets(t, map[string]http.HandlerFunc{
		"one": func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`not-json`))
		},
		"two": func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"items":[]}`))
		},
	})
	defer cleanup()

	p, err := newTestProxy(targets, targets[0])
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/configmaps", http.NoBody)
	rec := httptest.NewRecorder()
	serveTestHTTP(p, rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusBadGateway, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "decode list response") {
		t.Fatalf("body = %s, want decode list response error", rec.Body.String())
	}
}

func TestResourcePathContainingLogSegmentIsNotLongRunning(t *testing.T) {
	targets, cleanup := testTargets(t, map[string]http.HandlerFunc{
		"one": func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"apiVersion":"observability.example.com/v1","kind":"LoggingConfigList","items":[{"metadata":{"name":"a"}}]}`))
		},
		"two": func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"apiVersion":"observability.example.com/v1","kind":"LoggingConfigList","items":[{"metadata":{"name":"b"}}]}`))
		},
	})
	defer cleanup()

	p, err := newTestProxy(targets, targets[0])
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/apis/observability.example.com/v1/namespaces/default/loggingconfigs", http.NoBody)
	rec := httptest.NewRecorder()
	serveTestHTTP(p, rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}

	var payload struct {
		Items []struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
		} `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Items) != 2 {
		t.Fatalf("items = %d, want aggregated items from both targets; body=%s", len(payload.Items), rec.Body.String())
	}
}

func TestHelmStorageListUsesPrimaryOnly(t *testing.T) {
	calls := &callRecorder{}
	targets, cleanup := testTargets(t, map[string]http.HandlerFunc{
		"one": func(w http.ResponseWriter, _ *http.Request) {
			calls.add("one")
			_, _ = w.Write([]byte(`{"apiVersion":"v1","kind":"SecretList","items":[{"metadata":{"name":"sh.helm.release.v1.demo.v1"}}]}`))
		},
		"two": func(w http.ResponseWriter, _ *http.Request) {
			calls.add("two")
			_, _ = w.Write([]byte(`{"apiVersion":"v1","kind":"SecretList","items":[{"metadata":{"name":"sh.helm.release.v1.demo.v1"}}]}`))
		},
	})
	defer cleanup()

	p, err := newTestProxyWithOptions(targets, targets[0], Options{HelmReleaseProxy: true})
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/namespaces/default/secrets?labelSelector=owner%3Dhelm%2Cname%3Ddemo", http.NoBody)
	rec := httptest.NewRecorder()
	serveTestHTTP(p, rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	gotCalls := calls.snapshot()
	if len(gotCalls) != 1 || gotCalls[0] != "one" {
		t.Fatalf("calls = %v, want primary target only", gotCalls)
	}
}

func TestHelmStorageWatchUsesPrimaryOnly(t *testing.T) {
	calls := &callRecorder{}
	targets, cleanup := testTargets(t, map[string]http.HandlerFunc{
		"one": func(w http.ResponseWriter, _ *http.Request) {
			calls.add("one")
			_, _ = w.Write([]byte(`{"type":"ADDED","object":{"metadata":{"name":"release"}}}` + "\n"))
		},
		"two": func(w http.ResponseWriter, _ *http.Request) {
			calls.add("two")
			_, _ = w.Write([]byte(`{"type":"ADDED","object":{"metadata":{"name":"release"}}}` + "\n"))
		},
	})
	defer cleanup()

	p, err := newTestProxyWithOptions(targets, targets[0], Options{HelmReleaseProxy: true})
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/namespaces/default/secrets?watch=true&labelSelector=owner%3Dhelm", http.NoBody)
	rec := httptest.NewRecorder()
	serveTestHTTP(p, rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	gotCalls := calls.snapshot()
	if !slices.Equal(gotCalls, []string{"one"}) {
		t.Fatalf("calls = %v, want primary target only", gotCalls)
	}
}

func TestHelmStorageListAggregatesByDefault(t *testing.T) {
	targets, cleanup := testTargets(t, map[string]http.HandlerFunc{
		"one": func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"apiVersion":"v1","kind":"SecretList","items":[{"metadata":{"name":"sh.helm.release.v1.demo.v1"}}]}`))
		},
		"two": func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"apiVersion":"v1","kind":"SecretList","items":[{"metadata":{"name":"sh.helm.release.v1.demo.v1"}}]}`))
		},
	})
	defer cleanup()

	p, err := newTestProxy(targets, targets[0])
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/namespaces/default/secrets?labelSelector=owner%3Dhelm%2Cname%3Ddemo", http.NoBody)
	rec := httptest.NewRecorder()
	serveTestHTTP(p, rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}

	var payload struct {
		Items []struct {
			Metadata struct {
				Labels map[string]string `json:"labels"`
			} `json:"metadata"`
		} `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Items) != 2 {
		t.Fatalf("items = %d, want aggregated Helm release items by default; body=%s", len(payload.Items), rec.Body.String())
	}
	if payload.Items[0].Metadata.Labels["context"] != "one" || payload.Items[1].Metadata.Labels["context"] != "two" {
		t.Fatalf("items = %#v, want context labels from both targets", payload.Items)
	}
}

func TestHelmStorageListRequestClassification(t *testing.T) {
	tests := []struct {
		name string
		req  *http.Request
		want bool
	}{
		{
			name: "namespaced secrets with helm owner",
			req:  httptest.NewRequest(http.MethodGet, "/api/v1/namespaces/default/secrets?labelSelector=owner%3Dhelm", http.NoBody),
			want: true,
		},
		{
			name: "cluster configmaps with double equals selector",
			req:  httptest.NewRequest(http.MethodGet, "/api/v1/configmaps?labelSelector=name%3Ddemo,owner%3D%3Dhelm", http.NoBody),
			want: true,
		},
		{
			name: "post is not a helm list",
			req:  httptest.NewRequest(http.MethodPost, "/api/v1/secrets?labelSelector=owner%3Dhelm", http.NoBody),
		},
		{
			name: "pods are not helm storage",
			req:  httptest.NewRequest(http.MethodGet, "/api/v1/pods?labelSelector=owner%3Dhelm", http.NoBody),
		},
		{
			name: "selector without helm owner",
			req:  httptest.NewRequest(http.MethodGet, "/api/v1/secrets?labelSelector=owner%3Dnot-helm", http.NoBody),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isHelmStorageListRequest(tt.req); got != tt.want {
				t.Fatalf("isHelmStorageListRequest = %t, want %t", got, tt.want)
			}
		})
	}
}

func TestReadOnlyRejectsMutations(t *testing.T) {
	calls := &callRecorder{}
	targets, cleanup := testTargets(t, map[string]http.HandlerFunc{
		"one": func(w http.ResponseWriter, r *http.Request) {
			calls.add("one:" + r.Method)
			_, _ = w.Write([]byte(`{"gitVersion":"v1.32.0"}`))
		},
		"two": func(w http.ResponseWriter, r *http.Request) {
			calls.add("two:" + r.Method)
			_, _ = w.Write([]byte(`{"gitVersion":"v1.32.0"}`))
		},
	})
	defer cleanup()

	p, err := newTestProxyWithOptions(targets, targets[0], Options{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}

	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
		t.Run(method, func(t *testing.T) {
			req := httptest.NewRequest(method, "/api/v1/namespaces/default/configmaps/demo", strings.NewReader(`{"data":{"key":"value"}}`))
			rec := httptest.NewRecorder()
			serveTestHTTP(p, rec, req)

			if rec.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusForbidden, rec.Body.String())
			}
		})
	}

	if gotCalls := calls.snapshot(); len(gotCalls) != 0 {
		t.Fatalf("upstream calls = %v, want no upstream calls for read-only mutations", gotCalls)
	}

	req := httptest.NewRequest(http.MethodGet, "/version", http.NoBody)
	rec := httptest.NewRecorder()
	serveTestHTTP(p, rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("read status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
}

func TestFanOutPropagatesUpstreamFailure(t *testing.T) {
	targets, cleanup := testTargets(t, map[string]http.HandlerFunc{
		"one": func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "nope", http.StatusConflict)
		},
		"two": func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"ok":true}`))
		},
	})
	defer cleanup()

	p, err := newTestProxy(targets, targets[0])
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/namespaces/default/configmaps", strings.NewReader(`{"metadata":{"name":"demo"}}`))
	rec := httptest.NewRecorder()
	serveTestHTTP(p, rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusConflict, rec.Body.String())
	}
}

func TestFanOutRejectsUnreadableBody(t *testing.T) {
	target := Target{Name: "one", Host: mustParseURL(t, "https://one.example.test"), Client: http.DefaultClient}
	p, err := newTestProxy([]Target{target}, target)
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/namespaces/default/configmaps", http.NoBody)
	req.Body = errReadCloser{err: errors.New("read failed")}
	rec := httptest.NewRecorder()
	serveTestHTTP(p, rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusBadRequest, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "read failed") {
		t.Fatalf("body = %s, want read failed error", rec.Body.String())
	}
}

func TestFanOutRejectsOversizedBodyBeforeUpstreamCalls(t *testing.T) {
	targets, cleanup := testTargets(t, map[string]http.HandlerFunc{
		"one": func(http.ResponseWriter, *http.Request) {
			t.Fatal("oversized mutation must not reach upstream")
		},
	})
	defer cleanup()
	p, err := newTestProxy(targets, targets[0])
	if err != nil {
		t.Fatal(err)
	}

	body := bytes.NewReader(make([]byte, maxMutationRequestBodyBytes+1))
	req := httptest.NewRequest(http.MethodPost, "/api/v1/namespaces/default/configmaps", body)
	rec := httptest.NewRecorder()
	serveTestHTTP(p, rec, req)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusRequestEntityTooLarge, rec.Body.String())
	}
}

func TestNamedResourceGetRoutesToTargetContainingObject(t *testing.T) {
	targets, cleanup := testTargets(t, map[string]http.HandlerFunc{
		"one": func(w http.ResponseWriter, r *http.Request) {
			http.NotFound(w, r)
		},
		"two": func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"metadata":{"name":"demo","labels":{"real":"two"}}}`))
		},
	})
	defer cleanup()

	p, err := newTestProxy(targets, targets[0])
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/namespaces/default/pods/demo", http.NoBody)
	rec := httptest.NewRecorder()
	serveTestHTTP(p, rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"real":"two"`) {
		t.Fatalf("body = %s, want object from second target", rec.Body.String())
	}
}

func TestNamedResourceGetFallsBackToPrimaryWhenNoTargetContainsObject(t *testing.T) {
	calls := &callRecorder{}
	targets, cleanup := testTargets(t, map[string]http.HandlerFunc{
		"one": func(w http.ResponseWriter, r *http.Request) {
			calls.add("one:" + r.Method + ":" + r.URL.Path)
			if len(calls.snapshot()) == 1 {
				http.NotFound(w, r)
				return
			}
			_, _ = w.Write([]byte(`{"metadata":{"name":"demo"}}`))
		},
		"two": func(w http.ResponseWriter, r *http.Request) {
			calls.add("two:" + r.Method + ":" + r.URL.Path)
			http.NotFound(w, r)
		},
	})
	defer cleanup()

	p, err := newTestProxy(targets, targets[0])
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/namespaces/default/configmaps/demo", http.NoBody)
	rec := httptest.NewRecorder()
	serveTestHTTP(p, rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	gotCalls := calls.snapshot()
	if !slices.Equal(gotCalls, []string{
		"one:GET:/api/v1/namespaces/default/configmaps/demo",
		"two:GET:/api/v1/namespaces/default/configmaps/demo",
		"one:GET:/api/v1/namespaces/default/configmaps/demo",
	}) {
		t.Fatalf("calls = %v, want lookup on both targets then primary fallback", gotCalls)
	}
}

func TestRetriesTemporaryUpstreamFailures(t *testing.T) {
	var attempts int32
	targets, cleanup := testTargets(t, map[string]http.HandlerFunc{
		"one": func(w http.ResponseWriter, _ *http.Request) {
			if atomic.AddInt32(&attempts, 1) == 1 {
				http.Error(w, "try again", http.StatusServiceUnavailable)
				return
			}
			_, _ = w.Write([]byte(`{"gitVersion":"v1.32.0"}`))
		},
		"two": func(_ http.ResponseWriter, _ *http.Request) {
			t.Fatalf("secondary target should not be called for discovery requests")
		},
	})
	defer cleanup()

	p, err := newTestProxyWithOptions(targets, targets[0], Options{Retries: 1})
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/version", http.NoBody)
	rec := httptest.NewRecorder()
	serveTestHTTP(p, rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if attempts != 2 {
		t.Fatalf("attempts = %d, want 2", attempts)
	}
}

func TestRequestTimeout(t *testing.T) {
	targets, cleanup := testTargets(t, map[string]http.HandlerFunc{
		"one": func(w http.ResponseWriter, _ *http.Request) {
			time.Sleep(50 * time.Millisecond)
			_, _ = w.Write([]byte(`{"gitVersion":"v1.32.0"}`))
		},
		"two": func(_ http.ResponseWriter, _ *http.Request) {
			t.Fatalf("secondary target should not be called for discovery requests")
		},
	})
	defer cleanup()

	p, err := newTestProxyWithOptions(targets, targets[0], Options{RequestTimeout: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/version", http.NoBody)
	rec := httptest.NewRecorder()
	serveTestHTTP(p, rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusBadGateway, rec.Body.String())
	}
}

func decodeUpdate(t *testing.T, target string, r *http.Request) seenUpdate {
	t.Helper()

	var payload struct {
		Metadata struct {
			UID             string `json:"uid"`
			ResourceVersion string `json:"resourceVersion"`
		} `json:"metadata"`
	}
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	return seenUpdate{
		target:          target,
		uid:             payload.Metadata.UID,
		resourceVersion: payload.Metadata.ResourceVersion,
	}
}

func TestRequestTimeoutDoesNotCloseOpenedWatch(t *testing.T) {
	calls := &callRecorder{}
	targets, cleanup := testTargets(t, map[string]http.HandlerFunc{
		"one": func(w http.ResponseWriter, _ *http.Request) {
			calls.add("one")
			w.WriteHeader(http.StatusOK)
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
			time.Sleep(20 * time.Millisecond)
			_, _ = w.Write([]byte(`{"type":"MODIFIED","object":{"metadata":{"name":"demo"}}}` + "\n"))
		},
		"two": func(w http.ResponseWriter, _ *http.Request) {
			calls.add("two")
			_, _ = w.Write([]byte(`{"type":"MODIFIED","object":{"metadata":{"name":"demo"}}}` + "\n"))
		},
	})
	defer cleanup()

	p, err := newTestProxyWithOptions(targets, targets[0], Options{RequestTimeout: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/apis/apps/v1/namespaces/default/deployments?watch=true", http.NoBody)
	rec := httptest.NewRecorder()
	serveTestHTTP(p, rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	gotCalls := calls.snapshot()
	for _, target := range []string{"one", "two"} {
		if !slices.Contains(gotCalls, target) {
			t.Fatalf("calls = %v, want watch request to %s", gotCalls, target)
		}
		if !strings.Contains(rec.Body.String(), `"context":"`+target+`"`) {
			t.Fatalf("body = %s, want context label for %s", rec.Body.String(), target)
		}
	}
}

func TestOpenWatchStreamReturnsUpstreamStatusFailure(t *testing.T) {
	targets, cleanup := testTargets(t, map[string]http.HandlerFunc{
		"one": func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "missing", http.StatusNotFound)
		},
		"two": func(_ http.ResponseWriter, _ *http.Request) {
			t.Fatal("second target should not be used")
		},
	})
	defer cleanup()

	p, err := newTestProxy(targets, targets[0])
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/pods?watch=true", http.NoBody)

	result := p.openWatchStream(context.Background(), req, targets[0])
	if !result.failed {
		t.Fatal("openWatchStream succeeded, want failure")
	}
	if result.response.status != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", result.response.status, http.StatusNotFound)
	}
	if !strings.Contains(string(result.response.body), "missing") {
		t.Fatalf("body = %q, want missing", string(result.response.body))
	}
}

func TestOpenWatchStreamUsesRequestTimeout(t *testing.T) {
	targets, cleanup := testTargets(t, map[string]http.HandlerFunc{
		"one": func(w http.ResponseWriter, _ *http.Request) {
			time.Sleep(100 * time.Millisecond)
			_, _ = w.Write([]byte(`{"type":"BOOKMARK"}` + "\n"))
		},
		"two": func(_ http.ResponseWriter, _ *http.Request) {
			t.Fatal("second target should not be used")
		},
	})
	defer cleanup()

	p, err := newTestProxyWithOptions(targets, targets[0], Options{RequestTimeout: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/pods?watch=true", http.NoBody)

	result := p.openWatchStream(context.Background(), req, targets[0])
	if !result.failed {
		t.Fatal("openWatchStream succeeded, want timeout failure")
	}
	if !errors.Is(result.response.err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want context deadline exceeded", result.response.err)
	}
}

func TestWatchOpenUsesTimeoutAndStartsTargetsInParallel(t *testing.T) {
	var calls int32
	targets, cleanup := testTargets(t, map[string]http.HandlerFunc{
		"one": func(_ http.ResponseWriter, r *http.Request) {
			atomic.AddInt32(&calls, 1)
			<-r.Context().Done()
		},
		"two": func(_ http.ResponseWriter, r *http.Request) {
			atomic.AddInt32(&calls, 1)
			<-r.Context().Done()
		},
	})
	defer cleanup()

	p, err := newTestProxyWithOptions(targets, targets[0], Options{RequestTimeout: 20 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/apis/apps/v1/namespaces/default/deployments?watch=true", http.NoBody)
	rec := httptest.NewRecorder()
	start := time.Now()
	serveTestHTTP(p, rec, req)
	elapsed := time.Since(start)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusBadGateway, rec.Body.String())
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("calls = %d, want both watch opens to start", got)
	}
	if elapsed > 200*time.Millisecond {
		t.Fatalf("watch open took %s, want bounded by request timeout", elapsed)
	}
}

func TestPodLogsRouteToTargetContainingPod(t *testing.T) {
	targets, cleanup := testTargets(t, map[string]http.HandlerFunc{
		"one": func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/api/v1/namespaces/default/pods/demo" {
				http.NotFound(w, r)
				return
			}
			t.Fatalf("primary target should not receive log stream, got %s", r.URL.Path)
		},
		"two": func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/api/v1/namespaces/default/pods/demo":
				_, _ = w.Write([]byte(`{"metadata":{"name":"demo"}}`))
			case "/api/v1/namespaces/default/pods/demo/log":
				_, _ = w.Write([]byte("hello from target two\n"))
			default:
				t.Fatalf("unexpected path %s", r.URL.Path)
			}
		},
	})
	defer cleanup()

	p, err := newTestProxy(targets, targets[0])
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/namespaces/default/pods/demo/log", http.NoBody)
	rec := httptest.NewRecorder()
	serveTestHTTP(p, rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if rec.Body.String() != "hello from target two\n" {
		t.Fatalf("body = %q, want target two logs", rec.Body.String())
	}
}

func TestPodExecUpgradeIsProxiedBidirectionally(t *testing.T) {
	targets, cleanup := testTargets(t, podExecUpgradeHandlers(t))
	defer cleanup()

	p, err := newTestProxy(targets, targets[0])
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(p)
	defer server.Close()

	resp, err := server.Client().Do(newPodExecUpgradeRequest(t, server.URL))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	assertPodExecUpgradeResponse(t, resp)
}

func podExecUpgradeHandlers(t *testing.T) map[string]http.HandlerFunc {
	t.Helper()

	return map[string]http.HandlerFunc{
		"one": rejectPrimaryPodExecHandler(t),
		"two": podExecTargetHandler(t),
	}
}

func rejectPrimaryPodExecHandler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/namespaces/default/pods/demo" {
			http.NotFound(w, r)
			return
		}
		t.Fatalf("primary target should not receive exec stream, got %s", r.URL.Path)
	}
}

func podExecTargetHandler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/namespaces/default/pods/demo":
			_, _ = w.Write([]byte(`{"metadata":{"name":"demo"}}`))
		case "/api/v1/namespaces/default/pods/demo/exec":
			assertPodExecUpgradeRequest(t, r)
			writePodExecUpgradeResponse(t, w)
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}
}

func assertPodExecUpgradeRequest(t *testing.T, r *http.Request) {
	t.Helper()

	if r.Header.Get("Authorization") != "" {
		t.Fatalf("proxy Authorization header leaked upstream: %q", r.Header.Get("Authorization"))
	}
	if !strings.EqualFold(r.Header.Get("Upgrade"), "spdy/3.1") {
		t.Fatalf("upgrade = %q, want spdy/3.1", r.Header.Get("Upgrade"))
	}
}

func writePodExecUpgradeResponse(t *testing.T, w http.ResponseWriter) {
	t.Helper()

	hijacker, ok := w.(http.Hijacker)
	if !ok {
		t.Fatalf("response writer does not support hijacking")
	}
	conn, rw, err := hijacker.Hijack()
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	_, _ = fmt.Fprintf(rw, "HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: spdy/3.1\r\n\r\nupgraded\n")
	_ = rw.Flush()
}

func newPodExecUpgradeRequest(t *testing.T, serverURL string) *http.Request {
	t.Helper()

	req, err := http.NewRequest(http.MethodGet, serverURL+"/api/v1/namespaces/default/pods/demo/exec", http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+testBearerToken)
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "spdy/3.1")
	return req
}

func assertPodExecUpgradeResponse(t *testing.T, resp *http.Response) {
	t.Helper()

	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusSwitchingProtocols)
	}
	if !strings.EqualFold(resp.Header.Get("Upgrade"), "spdy/3.1") {
		t.Fatalf("response upgrade = %q, want spdy/3.1", resp.Header.Get("Upgrade"))
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "upgraded\n" {
		t.Fatalf("body = %q, want upgraded stream data", string(body))
	}
}

func TestRejectsMissingBearerToken(t *testing.T) {
	targets, cleanup := testTargets(t, map[string]http.HandlerFunc{
		"one": func(_ http.ResponseWriter, _ *http.Request) {
			t.Fatalf("upstream should not be called without bearer token")
		},
		"two": func(_ http.ResponseWriter, _ *http.Request) {
			t.Fatalf("upstream should not be called without bearer token")
		},
	})
	defer cleanup()

	p, err := newTestProxy(targets, targets[0])
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/version", http.NoBody)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusUnauthorized, rec.Body.String())
	}
}

func TestAcceptsBearerToken(t *testing.T) {
	targets, cleanup := testTargets(t, map[string]http.HandlerFunc{
		"one": func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"gitVersion":"v1.32.0"}`))
		},
		"two": func(_ http.ResponseWriter, _ *http.Request) {
			t.Fatalf("secondary target should not be called for discovery requests")
		},
	})
	defer cleanup()

	p, err := newTestProxy(targets, targets[0])
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/version", http.NoBody)
	rec := httptest.NewRecorder()
	serveTestHTTP(p, rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
}

func TestSleepWithContext(t *testing.T) {
	if !sleepWithContext(context.Background(), 0) {
		t.Fatal("sleepWithContext(0) = false, want true")
	}
	if !sleepWithContext(context.Background(), time.Nanosecond) {
		t.Fatal("sleepWithContext(short delay) = false, want true")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if sleepWithContext(ctx, time.Second) {
		t.Fatal("sleepWithContext(canceled) = true, want false")
	}
}

func TestSingleJoiningSlash(t *testing.T) {
	if got := singleJoiningSlash("/api/", "/v1"); got != "/api/v1" {
		t.Fatalf("singleJoiningSlash double slash = %q, want /api/v1", got)
	}
	if got := singleJoiningSlash("/api", "v1"); got != "/api/v1" {
		t.Fatalf("singleJoiningSlash missing slash = %q, want /api/v1", got)
	}
	if got := singleJoiningSlash("/api", "/v1"); got != "/api/v1" {
		t.Fatalf("singleJoiningSlash existing slash = %q, want /api/v1", got)
	}
}

func TestMarkWatchEventSourceEdgeCases(t *testing.T) {
	line := []byte(`{"type":"ADDED"}` + "\n")
	if got := string(markWatchEventSource(line, "one")); got != string(line) {
		t.Fatalf("watch event without object = %q, want original", got)
	}
	line = []byte(`{"object":{"spec":{}}}` + "\n")
	if got := string(markWatchEventSource(line, "one")); got != string(line) {
		t.Fatalf("watch event without metadata = %q, want original", got)
	}
	if got := string(markWatchEventSource([]byte("not-json\n"), "one")); got != "not-json\n" {
		t.Fatalf("invalid watch event = %q, want original", got)
	}
	if got := string(markWatchEventSource([]byte("\n"), "one")); got != "\n" {
		t.Fatalf("blank watch event = %q, want original newline", got)
	}
}

func TestSmallRequestPathHelpers(t *testing.T) {
	if !isLongRunningRequest(httptest.NewRequest(http.MethodGet, "/api/v1/pods?watch=true", http.NoBody)) {
		t.Fatal("watch request should be long-running")
	}
	if !isNamedResourcePath("/api/v1/nodes/node-a") {
		t.Fatal("core cluster-scoped named resource path was not detected")
	}
	if !isNamedResourcePath("/apis/apps/v1/deployments/demo") {
		t.Fatal("apis cluster-scoped named resource path was not detected")
	}
}

func TestAggregateResourceVersionEdgeCases(t *testing.T) {
	if _, ok := decodeAggregateResourceVersion("plain-resource-version"); ok {
		t.Fatal("plain resourceVersion decoded as aggregate")
	}
	if _, ok := decodeAggregateResourceVersion(aggregateResourceVersionPrefix + "not-base64"); ok {
		t.Fatal("invalid base64 aggregate resourceVersion decoded")
	}
	if _, ok := decodeAggregateResourceVersion(aggregateResourceVersionPrefix + "W10"); ok {
		t.Fatal("non-object aggregate resourceVersion decoded")
	}
	upstreamURL := mustParseURL(t, "https://one.example.test/api/v1/pods?resourceVersion="+encodeAggregateResourceVersion(map[string]string{"two": "22"}))
	applyAggregateResourceVersion(upstreamURL, "one")
	if got := upstreamURL.Query().Get("resourceVersion"); got != "" {
		t.Fatalf("resourceVersion = %q, want removed for missing target", got)
	}
}

func TestObjectDecodeAndRewriteEdgeCases(t *testing.T) {
	if _, err := decodeObject([]byte(":\n:")); err == nil {
		t.Fatal("decodeObject returned nil error for invalid YAML")
	}
	if got, err := rewriteObjectIdentity([]byte("not-json"), []byte(`{"metadata":{"uid":"1"}}`)); err != nil || string(got) != "not-json" {
		t.Fatalf("rewriteObjectIdentity invalid desired = %q, %v; want original body", string(got), err)
	}
	if got, err := rewriteObjectIdentity([]byte(`{"metadata":{"name":"demo"}}`), []byte(`{"kind":"ConfigMap"}`)); err != nil || string(got) != `{"metadata":{"name":"demo"}}` {
		t.Fatalf("rewriteObjectIdentity without current metadata = %q, %v; want original body", string(got), err)
	}
}

func TestForwardSingleReturnsBadGatewayOnTransportError(t *testing.T) {
	upstreamErr := errors.New("upstream unavailable")
	target := Target{
		Name: "one",
		Host: mustParseURL(t, "https://one.example.test"),
		Client: &http.Client{Transport: roundTripperFunc(func(*http.Request) (*http.Response, error) {
			return nil, upstreamErr
		})},
	}
	p, err := newTestProxy([]Target{target}, target)
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/version", http.NoBody)
	rec := httptest.NewRecorder()
	serveTestHTTP(p, rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusBadGateway, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), upstreamErr.Error()) {
		t.Fatalf("body = %s, want transport error", rec.Body.String())
	}
}

func TestBodyForTargetKeepsOriginalBodyWhenObjectIsMissing(t *testing.T) {
	targets, cleanup := testTargets(t, map[string]http.HandlerFunc{
		"one": func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodGet {
				t.Fatalf("method = %s, want GET", r.Method)
			}
			http.NotFound(w, r)
		},
		"two": func(http.ResponseWriter, *http.Request) {},
	})
	defer cleanup()

	p, err := newTestProxy(targets, targets[0])
	if err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"metadata":{"name":"demo","uid":"old"}}`)
	req := httptest.NewRequest(http.MethodPut, "/api/v1/namespaces/default/configmaps/demo", http.NoBody)

	got, err := p.bodyForTarget(context.Background(), targets[0], req, body)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, body) {
		t.Fatalf("body = %s, want original %s", got, body)
	}
}

func TestTargetsForExistingResourceMutationRejectsUnexpectedLookupStatus(t *testing.T) {
	targets, cleanup := testTargets(t, map[string]http.HandlerFunc{
		"one": func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "forbidden", http.StatusForbidden)
		},
		"two": func(http.ResponseWriter, *http.Request) {},
	})
	defer cleanup()

	p, err := newTestProxy(targets, targets[0])
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPatch, "/api/v1/namespaces/default/configmaps/demo", http.NoBody)

	_, _, err = p.targetsForExistingResourceMutation(context.Background(), req, req.URL.Path)
	if err == nil || !strings.Contains(err.Error(), "one: get existing resource before mutation returned HTTP 403") {
		t.Fatalf("error = %v, want lookup status error", err)
	}
}

func TestSelectedWatchIsEmptyRejectsInvalidListPayload(t *testing.T) {
	targets, cleanup := testTargets(t, map[string]http.HandlerFunc{
		"one": func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte("not-json"))
		},
		"two": func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"items":[]}`))
		},
	})
	defer cleanup()

	p, err := newTestProxy(targets, targets[0])
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/pods?watch=true&fieldSelector=metadata.name=demo", http.NoBody)

	empty, failed := p.isNamedWatchEmptyAcrossTargets(req)
	if empty || failed == nil || failed.err == nil {
		t.Fatalf("result = empty:%t failed:%#v, want invalid payload failure", empty, failed)
	}
}

func TestOpenWatchStreamReturnsTransportError(t *testing.T) {
	upstreamErr := errors.New("watch transport failed")
	target := Target{
		Name: "one",
		Host: mustParseURL(t, "https://one.example.test"),
		Client: &http.Client{Transport: roundTripperFunc(func(*http.Request) (*http.Response, error) {
			return nil, upstreamErr
		})},
	}
	p, err := newTestProxy([]Target{target}, target)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/pods?watch=true", http.NoBody)

	result := p.openWatchStream(context.Background(), req, target)
	if !result.failed || !errors.Is(result.response.err, upstreamErr) {
		t.Fatalf("result = %#v, want failed transport error", result)
	}
}

func TestMergeTableRowsWithoutEmbeddedObject(t *testing.T) {
	merged, err := mergeLists([]upstreamResponse{{
		target: Target{Name: "one"},
		body:   []byte(`{"kind":"Table","rows":[{"cells":["demo"]}]}`),
	}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(merged), `"rows":[{"cells":["demo"]}]`) {
		t.Fatalf("merged = %s, want original row", merged)
	}
}

func TestFirstTargetByNameIgnoresTargetOrder(t *testing.T) {
	targets := []Target{{Name: "beta"}, {Name: "alpha"}}
	p := &Proxy{targets: targets}
	if got := p.firstTargetByName(); got.Name != "alpha" {
		t.Fatalf("first target = %q, want alpha", got.Name)
	}
}

func TestResourceAnnotationsHandlesMissingMetadata(t *testing.T) {
	if got := resourceAnnotations([]byte(`{"kind":"ConfigMap"}`)); got != nil {
		t.Fatalf("annotations = %v, want nil", got)
	}
	if got := resourceAnnotations([]byte(`{"metadata":{"annotations":{"answer":42}}}`)); len(got) != 0 {
		t.Fatalf("annotations = %v, want no non-string values", got)
	}
}

func newTestProxy(targets []Target, primary Target) (*Proxy, error) {
	return newTestProxyWithOptions(targets, primary, Options{})
}

func newTestProxyWithOptions(targets []Target, primary Target, options Options) (*Proxy, error) {
	options.BearerToken = testBearerToken
	return NewWithOptions(targets, primary, options)
}

func serveTestHTTP(p *Proxy, rec *httptest.ResponseRecorder, req *http.Request) {
	req.Header.Set("Authorization", "Bearer "+testBearerToken)
	p.ServeHTTP(rec, req)
}

func gzipListHandler(t *testing.T, body string) http.HandlerFunc {
	t.Helper()

	return func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
			t.Fatalf("upstream Accept-Encoding = %q, want gzip from Go transport", r.Header.Get("Accept-Encoding"))
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Encoding", "gzip")
		gz := gzip.NewWriter(w)
		_, err := gz.Write([]byte(body))
		if closeErr := gz.Close(); err == nil {
			err = closeErr
		}
		if err != nil {
			t.Fatal(err)
		}
	}
}

func paginatedListHandler(t *testing.T, resourceVersion string, names []string) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		limit, err := strconv.Atoi(r.URL.Query().Get("limit"))
		if err != nil || limit <= 0 {
			t.Errorf("upstream limit = %q, want positive integer", r.URL.Query().Get("limit"))
			http.Error(w, "invalid limit", http.StatusBadRequest)
			return
		}
		start := 0
		if continueToken := r.URL.Query().Get("continue"); continueToken != "" {
			if !strings.HasPrefix(continueToken, "offset-") {
				t.Errorf("upstream continue = %q, want target-local token", continueToken)
				http.Error(w, "invalid continue", http.StatusBadRequest)
				return
			}
			start, err = strconv.Atoi(strings.TrimPrefix(continueToken, "offset-"))
			if err != nil || start < 0 || start > len(names) {
				t.Errorf("upstream continue = %q, want valid offset", continueToken)
				http.Error(w, "invalid continue", http.StatusBadRequest)
				return
			}
		}
		end := min(start+limit, len(names))
		items := make([]map[string]any, 0, end-start)
		for _, name := range names[start:end] {
			items = append(items, map[string]any{"metadata": map[string]any{"name": name}})
		}
		metadata := map[string]any{"resourceVersion": resourceVersion}
		if end < len(names) {
			metadata["continue"] = fmt.Sprintf("offset-%d", end)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"apiVersion": "v1",
			"kind":       "PodList",
			"metadata":   metadata,
			"items":      items,
		})
	}
}

func mapsEqual(left, right map[string]string) bool {
	if len(left) != len(right) {
		return false
	}
	for key, value := range left {
		if right[key] != value {
			return false
		}
	}
	return true
}

type errReadCloser struct {
	err error
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func (r errReadCloser) Read([]byte) (int, error) {
	return 0, r.err
}

func (errReadCloser) Close() error {
	return nil
}

func mustParseURL(t *testing.T, value string) *url.URL {
	t.Helper()

	parsed, err := url.Parse(value)
	if err != nil {
		t.Fatal(err)
	}
	return parsed
}

func testTargets(t *testing.T, handlers map[string]http.HandlerFunc) ([]Target, func()) {
	t.Helper()

	names := make([]string, 0, len(handlers))
	for name := range handlers {
		names = append(names, name)
	}
	slices.Sort(names)
	targets := make([]Target, 0, len(names))
	servers := make([]*httptest.Server, 0, len(names))
	for _, name := range names {
		server := httptest.NewServer(handlers[name])
		servers = append(servers, server)

		host, err := url.Parse(server.URL)
		if err != nil {
			t.Fatal(err)
		}
		targets = append(targets, Target{
			Name:   name,
			Host:   host,
			Client: server.Client(),
		})
	}

	return targets, func() {
		for _, server := range servers {
			server.Close()
		}
	}
}
