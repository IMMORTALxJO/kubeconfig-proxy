package proxy

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
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
		"one": func(w http.ResponseWriter, r *http.Request) {
			calls.add("one")
			_, _ = w.Write([]byte(`{"kind":"Job","metadata":{"name":"demo"}}`))
		},
		"two": func(w http.ResponseWriter, r *http.Request) {
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
		"one": func(w http.ResponseWriter, r *http.Request) {
			calls.add("one")
			_, _ = w.Write([]byte(`{"kind":"ConfigMap","metadata":{"name":"demo"}}`))
		},
		"two": func(w http.ResponseWriter, r *http.Request) {
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
			"annotations":{"kubeconfig.proxy/context-name":"two"}
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
		"one": func(w http.ResponseWriter, r *http.Request) {
			calls.add("one")
			_, _ = w.Write([]byte(`{"kind":"ConfigMap","metadata":{"name":"demo"}}`))
		},
		"two": func(w http.ResponseWriter, r *http.Request) {
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
			"annotations":{"kubeconfig.proxy/single-context":"true"}
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
		"one": func(w http.ResponseWriter, r *http.Request) {
			calls.add("one")
			_, _ = w.Write([]byte(`{"kind":"ConfigMap","metadata":{"name":"demo"}}`))
		},
		"two": func(w http.ResponseWriter, r *http.Request) {
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
    kubeconfig.proxy/single-context: "true"
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

func TestContextNameAnnotationRejectsUnknownTarget(t *testing.T) {
	targets, cleanup := testTargets(t, map[string]http.HandlerFunc{
		"one": func(w http.ResponseWriter, r *http.Request) {
			t.Fatalf("upstream should not be called")
		},
		"two": func(w http.ResponseWriter, r *http.Request) {
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
			"annotations":{"kubeconfig.proxy/context-name":"missing"}
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
		"one": func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"apiVersion":"v1","kind":"PodList","metadata":{"resourceVersion":"10"},"items":[{"metadata":{"name":"a"}}]}`))
		},
		"two": func(w http.ResponseWriter, r *http.Request) {
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

func TestAggregatesTableResponses(t *testing.T) {
	targets, cleanup := testTargets(t, map[string]http.HandlerFunc{
		"one": func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{
				"apiVersion":"meta.k8s.io/v1",
				"kind":"Table",
				"metadata":{"resourceVersion":"10"},
				"columnDefinitions":[{"name":"Name","type":"string","format":"","description":"name"}],
				"rows":[{"cells":["a"],"object":{"metadata":{"name":"a"}}}]
			}`))
		},
		"two": func(w http.ResponseWriter, r *http.Request) {
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

func TestResourcePathContainingLogSegmentIsNotLongRunning(t *testing.T) {
	targets, cleanup := testTargets(t, map[string]http.HandlerFunc{
		"one": func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"apiVersion":"observability.example.com/v1","kind":"LoggingConfigList","items":[{"metadata":{"name":"a"}}]}`))
		},
		"two": func(w http.ResponseWriter, r *http.Request) {
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
		"one": func(w http.ResponseWriter, r *http.Request) {
			calls.add("one")
			_, _ = w.Write([]byte(`{"apiVersion":"v1","kind":"SecretList","items":[{"metadata":{"name":"sh.helm.release.v1.demo.v1"}}]}`))
		},
		"two": func(w http.ResponseWriter, r *http.Request) {
			calls.add("two")
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
	gotCalls := calls.snapshot()
	if len(gotCalls) != 1 || gotCalls[0] != "one" {
		t.Fatalf("calls = %v, want primary target only", gotCalls)
	}
}

func TestNamedResourceGetRoutesToTargetContainingObject(t *testing.T) {
	targets, cleanup := testTargets(t, map[string]http.HandlerFunc{
		"one": func(w http.ResponseWriter, r *http.Request) {
			http.NotFound(w, r)
		},
		"two": func(w http.ResponseWriter, r *http.Request) {
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

func TestRetriesTemporaryUpstreamFailures(t *testing.T) {
	var attempts int32
	targets, cleanup := testTargets(t, map[string]http.HandlerFunc{
		"one": func(w http.ResponseWriter, r *http.Request) {
			if atomic.AddInt32(&attempts, 1) == 1 {
				http.Error(w, "try again", http.StatusServiceUnavailable)
				return
			}
			_, _ = w.Write([]byte(`{"gitVersion":"v1.32.0"}`))
		},
		"two": func(w http.ResponseWriter, r *http.Request) {
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
		"one": func(w http.ResponseWriter, r *http.Request) {
			time.Sleep(50 * time.Millisecond)
			_, _ = w.Write([]byte(`{"gitVersion":"v1.32.0"}`))
		},
		"two": func(w http.ResponseWriter, r *http.Request) {
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
		"one": func(w http.ResponseWriter, r *http.Request) {
			calls.add("one")
			w.WriteHeader(http.StatusOK)
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
			time.Sleep(20 * time.Millisecond)
			_, _ = w.Write([]byte(`{"type":"MODIFIED","object":{"metadata":{"name":"demo"}}}` + "\n"))
		},
		"two": func(w http.ResponseWriter, r *http.Request) {
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

func TestWatchOpenUsesTimeoutAndStartsTargetsInParallel(t *testing.T) {
	var calls int32
	targets, cleanup := testTargets(t, map[string]http.HandlerFunc{
		"one": func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt32(&calls, 1)
			<-r.Context().Done()
		},
		"two": func(w http.ResponseWriter, r *http.Request) {
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
	targets, cleanup := testTargets(t, map[string]http.HandlerFunc{
		"one": func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/api/v1/namespaces/default/pods/demo" {
				http.NotFound(w, r)
				return
			}
			t.Fatalf("primary target should not receive exec stream, got %s", r.URL.Path)
		},
		"two": func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/api/v1/namespaces/default/pods/demo":
				_, _ = w.Write([]byte(`{"metadata":{"name":"demo"}}`))
			case "/api/v1/namespaces/default/pods/demo/exec":
				if r.Header.Get("Authorization") != "" {
					t.Fatalf("proxy Authorization header leaked upstream: %q", r.Header.Get("Authorization"))
				}
				if !strings.EqualFold(r.Header.Get("Upgrade"), "spdy/3.1") {
					t.Fatalf("upgrade = %q, want spdy/3.1", r.Header.Get("Upgrade"))
				}

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
	server := httptest.NewServer(p)
	defer server.Close()

	req, err := http.NewRequest(http.MethodGet, server.URL+"/api/v1/namespaces/default/pods/demo/exec", http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+testBearerToken)
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "spdy/3.1")

	resp, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

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
		"one": func(w http.ResponseWriter, r *http.Request) {
			t.Fatalf("upstream should not be called without bearer token")
		},
		"two": func(w http.ResponseWriter, r *http.Request) {
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
		"one": func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"gitVersion":"v1.32.0"}`))
		},
		"two": func(w http.ResponseWriter, r *http.Request) {
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

func testTargets(t *testing.T, handlers map[string]http.HandlerFunc) ([]Target, func()) {
	t.Helper()

	names := []string{"one", "two"}
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
