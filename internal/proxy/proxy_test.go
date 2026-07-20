package proxy

import (
	"compress/gzip"
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

func TestDefaultRetries(t *testing.T) {
	if DefaultRetries != 5 {
		t.Fatalf("DefaultRetries = %d, want 5", DefaultRetries)
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

func TestHelmStorageListAggregatesByDefault(t *testing.T) {
	targets, cleanup := testTargets(t, map[string]http.HandlerFunc{
		"one": func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"apiVersion":"v1","kind":"SecretList","items":[{"metadata":{"name":"sh.helm.release.v1.demo.v1"}}]}`))
		},
		"two": func(w http.ResponseWriter, r *http.Request) {
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
