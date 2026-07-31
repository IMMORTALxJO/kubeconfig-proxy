package proxy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
)

func TestSingleContextMutationUsesPrimary(t *testing.T) {
	calls := &callLog{}
	p, cleanup := newProxy(t, "two", map[string]http.HandlerFunc{
		"one": calls.handler("one", `{"metadata":{"name":"demo"}}`),
		"two": calls.handler("two", `{"metadata":{"name":"demo"}}`),
	})
	defer cleanup()
	recorder := serve(p, http.MethodPost, "/api/v1/namespaces/default/configmaps", `{"metadata":{"annotations":{"kubeconfig-proxy.io/single-context":"true"}}}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", recorder.Code, recorder.Body.String())
	}
	if got := calls.names(); len(got) != 1 || got[0] != "two" {
		t.Fatalf("calls = %v, want [two]", got)
	}
}

func TestRequestResponsePostUsesPrimary(t *testing.T) {
	calls := &callLog{}
	p, cleanup := newProxy(t, "two", map[string]http.HandlerFunc{
		"one": calls.handler("one", `{"status":{"allowed":false}}`),
		"two": calls.handler("two", `{"status":{"allowed":true}}`),
	})
	defer cleanup()
	recorder := serve(p, http.MethodPost, "/apis/authorization.k8s.io/v1/selfsubjectaccessreviews", `{}`)
	if recorder.Code != http.StatusOK || !json.Valid(recorder.Body.Bytes()) {
		t.Fatalf("response = %d %s", recorder.Code, recorder.Body.String())
	}
	if got := calls.names(); len(got) != 1 || got[0] != "two" {
		t.Fatalf("calls = %v, want [two]", got)
	}
}

func TestTokenReviewUsesPrimary(t *testing.T) {
	calls := &callLog{}
	p, cleanup := newProxy(t, "two", map[string]http.HandlerFunc{
		"one": calls.handler("one", `{"status":{"authenticated":false}}`),
		"two": calls.handler("two", `{"status":{"authenticated":true}}`),
	})
	defer cleanup()

	recorder := serve(p, http.MethodPost, "/apis/authentication.k8s.io/v1/tokenreviews", `{"spec":{"token":"sensitive-token"}}`)
	if recorder.Code != http.StatusOK || !contains(recorder.Body.String(), `"authenticated":true`) {
		t.Fatalf("response = %d %s", recorder.Code, recorder.Body.String())
	}
	if got := calls.names(); len(got) != 1 || got[0] != "two" {
		t.Fatalf("calls = %v, want [two]", got)
	}
}

func TestNamedGetPrefersPrimary(t *testing.T) {
	calls := &callLog{}
	p, cleanup := newProxy(t, "two", map[string]http.HandlerFunc{
		"one": calls.handler("one", `{"metadata":{"labels":{"context":"one"}}}`),
		"two": calls.handler("two", `{"metadata":{"labels":{"context":"two"}}}`),
	})
	defer cleanup()
	recorder := serve(p, http.MethodGet, "/api/v1/namespaces/default/configmaps/demo", "")
	if recorder.Code != http.StatusOK || !contains(recorder.Body.String(), `"two"`) || !contains(recorder.Body.String(), `"kcp-context":"one,two"`) {
		t.Fatalf("response = %d %s", recorder.Code, recorder.Body.String())
	}
	if got := calls.names(); len(got) != 2 {
		t.Fatalf("calls = %v, want both contexts", got)
	}
}

func TestUnknownGetUsesPrimary(t *testing.T) {
	calls := &callLog{}
	p, cleanup := newProxy(t, "two", map[string]http.HandlerFunc{
		"one": calls.handler("one", `{}`), "two": calls.handler("two", `{}`),
	})
	defer cleanup()
	recorder := serve(p, http.MethodGet, "/metrics", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d", recorder.Code)
	}
	if got := calls.names(); len(got) != 1 || got[0] != "two" {
		t.Fatalf("calls = %v, want [two]", got)
	}
}

func TestAggregateListMarksSourceContext(t *testing.T) {
	p, cleanup := newProxy(t, "one", map[string]http.HandlerFunc{
		"one": func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"items":[{"metadata":{"name":"one"}}]}`))
		},
		"two": func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"items":[{"metadata":{"name":"two"}}]}`))
		},
	})
	defer cleanup()
	recorder := serve(p, http.MethodGet, "/api/v1/configmaps", "")
	if recorder.Code != http.StatusOK || !contains(recorder.Body.String(), `kubeconfig-proxy.io/source-context`) {
		t.Fatalf("response = %d %s", recorder.Code, recorder.Body.String())
	}
}

func TestAggregateListRequestsJSONInsteadOfProtobuf(t *testing.T) {
	protobufMediaType := "application/vnd.kubernetes.protobuf"
	p, cleanup := newProxy(t, "one", map[string]http.HandlerFunc{
		"one": jsonOnlyListHandler(protobufMediaType),
		"two": jsonOnlyListHandler(protobufMediaType),
	})
	defer cleanup()

	request := httptest.NewRequest(http.MethodGet, "/api/v1/configmaps", http.NoBody)
	request.Header.Set("Authorization", "Bearer test")
	request.Header.Set("Accept", "application/json;as=Table;g=meta.k8s.io;v=v1, "+protobufMediaType+";as=Table;g=meta.k8s.io;v=v1, application/json, "+protobufMediaType)
	recorder := httptest.NewRecorder()
	p.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
}

func jsonOnlyListHandler(protobufMediaType string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.Header.Get("Accept"), protobufMediaType) {
			w.Header().Set("Content-Type", protobufMediaType)
			_, _ = w.Write([]byte("not-json"))
			return
		}
		if !strings.Contains(r.Header.Get("Accept"), "application/json;as=Table;g=meta.k8s.io;v=v1") {
			http.Error(w, "JSON Table media range was removed", http.StatusBadRequest)
			return
		}
		_, _ = w.Write([]byte(`{"items":[]}`))
	}
}

func TestAggregateResourceVersionRoutesWatchPerTarget(t *testing.T) {
	var mu sync.Mutex
	watchResourceVersions := map[string]string{}
	p, cleanup := newProxy(t, "one", map[string]http.HandlerFunc{
		"one": resourceVersionHandler("one", "10", &mu, watchResourceVersions),
		"two": resourceVersionHandler("two", "20", &mu, watchResourceVersions),
	})
	defer cleanup()

	list := serve(p, http.MethodGet, "/api/v1/configmaps", "")
	if list.Code != http.StatusOK {
		t.Fatalf("list status = %d: %s", list.Code, list.Body.String())
	}
	var listResponse struct {
		Metadata struct {
			ResourceVersion string `json:"resourceVersion"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal(list.Body.Bytes(), &listResponse); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(listResponse.Metadata.ResourceVersion, "kubeconfig-proxy:") {
		t.Fatalf("resourceVersion = %q, want aggregate resource version", listResponse.Metadata.ResourceVersion)
	}

	watch := serve(p, http.MethodGet, "/api/v1/configmaps?watch=true&resourceVersion="+url.QueryEscape(listResponse.Metadata.ResourceVersion), "")
	if watch.Code != http.StatusOK {
		t.Fatalf("watch status = %d: %s", watch.Code, watch.Body.String())
	}
	mu.Lock()
	defer mu.Unlock()
	if got, want := watchResourceVersions["one"], "10"; got != want {
		t.Fatalf("one resourceVersion = %q, want %q", got, want)
	}
	if got, want := watchResourceVersions["two"], "20"; got != want {
		t.Fatalf("two resourceVersion = %q, want %q", got, want)
	}
}

func TestAggregatePaginatedResourceVersionRoutesWatchPerTarget(t *testing.T) {
	var mu sync.Mutex
	watchResourceVersions := map[string]string{}
	p, cleanup := newProxy(t, "one", map[string]http.HandlerFunc{
		"one": resourceVersionHandler("one", "10", &mu, watchResourceVersions),
		"two": resourceVersionHandler("two", "20", &mu, watchResourceVersions),
	})
	defer cleanup()

	list := serve(p, http.MethodGet, "/api/v1/configmaps?limit=500&resourceVersion=0", "")
	if list.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", list.Code, list.Body.String())
	}
	var listResponse struct {
		Metadata struct {
			ResourceVersion string `json:"resourceVersion"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal(list.Body.Bytes(), &listResponse); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(listResponse.Metadata.ResourceVersion, aggregateResourceVersionPrefix) {
		t.Fatalf("resourceVersion = %q, want aggregate resource version", listResponse.Metadata.ResourceVersion)
	}

	watch := serve(p, http.MethodGet, "/api/v1/configmaps?watch=true&resourceVersion="+url.QueryEscape(listResponse.Metadata.ResourceVersion), "")
	if watch.Code != http.StatusOK {
		t.Fatalf("watch status = %d: %s", watch.Code, watch.Body.String())
	}
	mu.Lock()
	defer mu.Unlock()
	if got, want := watchResourceVersions["one"], "10"; got != want {
		t.Fatalf("one resourceVersion = %q, want %q", got, want)
	}
	if got, want := watchResourceVersions["two"], "20"; got != want {
		t.Fatalf("two resourceVersion = %q, want %q", got, want)
	}
}

func TestCollectionWatchRoutesResourceVersionPerTarget(t *testing.T) {
	var mu sync.Mutex
	watchResourceVersions := map[string]string{}
	p, cleanup := newProxy(t, "one", map[string]http.HandlerFunc{
		"one": resourceVersionHandler("one", "10", &mu, watchResourceVersions),
		"two": resourceVersionHandler("two", "20", &mu, watchResourceVersions),
	})
	defer cleanup()

	watch := serve(p, http.MethodGet, "/api/v1/namespaces/default/configmaps?fieldSelector=metadata.name%3Ddemo&watch=true&resourceVersion=10", "")
	if watch.Code != http.StatusOK {
		t.Fatalf("watch status = %d: %s", watch.Code, watch.Body.String())
	}
	mu.Lock()
	defer mu.Unlock()
	if got, want := watchResourceVersions["one"], "10"; got != want {
		t.Fatalf("one resourceVersion = %q, want %q", got, want)
	}
	if got, want := watchResourceVersions["two"], "20"; got != want {
		t.Fatalf("two resourceVersion = %q, want %q", got, want)
	}
}

func TestNamedCollectionWatchUsesOnlyFoundContexts(t *testing.T) {
	var mu sync.Mutex
	watchPaths := map[string]string{}
	p, cleanup := newProxy(t, "one", map[string]http.HandlerFunc{
		"one": func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Query().Get("watch") != "true" {
				if r.URL.Path != "/api/v1/namespaces/default/configmaps/demo" {
					http.Error(w, "expected named object probe", http.StatusBadRequest)
					return
				}
				_, _ = w.Write([]byte(`{"metadata":{"name":"demo","resourceVersion":"10"}}`))
				return
			}
			mu.Lock()
			watchPaths["one"] = r.URL.Path + "?" + r.URL.RawQuery
			mu.Unlock()
			_, _ = w.Write([]byte(`{"type":"MODIFIED","object":{"metadata":{"name":"demo"}}}` + "\n"))
		},
		"two": func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Query().Get("watch") != "true" {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			mu.Lock()
			watchPaths["two"] = r.URL.Path + "?" + r.URL.RawQuery
			mu.Unlock()
			_, _ = w.Write([]byte(`{"type":"MODIFIED","object":{"metadata":{"name":"demo"}}}` + "\n"))
		},
	})
	defer cleanup()

	response := serve(p, http.MethodGet, "/api/v1/namespaces/default/configmaps?fieldSelector=metadata.name%3Ddemo&watch=true&resourceVersion=10", "")
	if response.Code != http.StatusOK {
		t.Fatalf("watch status = %d: %s", response.Code, response.Body.String())
	}
	mu.Lock()
	defer mu.Unlock()
	if got := watchPaths["one"]; !strings.HasPrefix(got, "/api/v1/namespaces/default/configmaps?") || !strings.Contains(got, "fieldSelector=metadata.name%3Ddemo") || !strings.Contains(got, "resourceVersion=10") {
		t.Fatalf("one watch = %q", got)
	}
	if got := watchPaths["two"]; got != "" {
		t.Fatalf("two watch = %q, want no watch for an absent object", got)
	}
}

func TestNamedWatchRequest(t *testing.T) {
	namedObject := httptest.NewRequest(http.MethodGet, "/api/v1/configmaps/demo?watch=true", http.NoBody)
	if request, ok := namedWatchRequest(namedObject, parseResourcePath(namedObject.URL.Path)); !ok || request != namedObject {
		t.Fatalf("named object request = %v, %t; want original request", request, ok)
	}

	collection := httptest.NewRequest(http.MethodGet, "/api/v1/configmaps?fieldSelector=metadata.name%3Ddemo&watch=true", http.NoBody)
	request, ok := namedWatchRequest(collection, parseResourcePath(collection.URL.Path))
	if !ok {
		t.Fatal("collection watch was not classified as named")
	}
	if request != collection {
		t.Fatalf("named collection request = %v, want original request", request)
	}

	for _, path := range []string{
		"/api/v1/configmaps?fieldSelector=metadata.namespace%3Ddefault&watch=true",
		"/api/v1/configmaps?fieldSelector=metadata.name&watch=true",
		"/api/v1/configmaps/demo/status?watch=true",
	} {
		request := httptest.NewRequest(http.MethodGet, path, http.NoBody)
		if named, ok := namedWatchRequest(request, parseResourcePath(request.URL.Path)); ok || named != nil {
			t.Fatalf("request %q = %v, %t; want not named", path, named, ok)
		}
	}
}

func TestNamedWatchProbeFailures(t *testing.T) {
	t.Run("all contexts absent", func(t *testing.T) {
		p, cleanup := newProxy(t, "one", map[string]http.HandlerFunc{
			"one": func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) },
			"two": func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) },
		})
		defer cleanup()

		response := serve(p, http.MethodGet, "/api/v1/configmaps/demo?watch=true", "")
		if response.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want %d", response.Code, http.StatusNotFound)
		}
	})

	t.Run("unexpected probe status", func(t *testing.T) {
		p, cleanup := newProxy(t, "one", map[string]http.HandlerFunc{
			"one": func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusInternalServerError) },
			"two": func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) },
		})
		defer cleanup()

		response := serve(p, http.MethodGet, "/api/v1/configmaps/demo?watch=true", "")
		if response.Code != http.StatusBadGateway || !contains(response.Body.String(), "existing object returned HTTP 500") {
			t.Fatalf("response = %d %s", response.Code, response.Body.String())
		}
	})

	t.Run("object has no resource version", func(t *testing.T) {
		p, cleanup := newProxy(t, "one", map[string]http.HandlerFunc{
			"one": func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{"metadata":{"name":"demo"}}`)) },
			"two": func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) },
		})
		defer cleanup()

		response := serve(p, http.MethodGet, "/api/v1/configmaps/demo?watch=true", "")
		if response.Code != http.StatusBadGateway || !contains(response.Body.String(), "list response has no resource version") {
			t.Fatalf("response = %d %s", response.Code, response.Body.String())
		}
	})
}

func resourceVersionHandler(name, resourceVersion string, mu *sync.Mutex, watchResourceVersions map[string]string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("watch") == "true" {
			mu.Lock()
			watchResourceVersions[name] = r.URL.Query().Get("resourceVersion")
			mu.Unlock()
			_, _ = w.Write([]byte(`{"type":"BOOKMARK","object":{"metadata":{}}}` + "\n"))
			return
		}
		_, _ = w.Write([]byte(`{"metadata":{"resourceVersion":"` + resourceVersion + `"},"items":[]}`))
	}
}

func TestAggregatePageSupportsTableResponses(t *testing.T) {
	p, cleanup := newProxy(t, "one", map[string]http.HandlerFunc{
		"one": func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"kind":"Table","metadata":{"resourceVersion":"10"},"rows":[{"object":{"metadata":{"name":"one"}}}]}`))
		},
		"two": func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"kind":"Table","metadata":{"resourceVersion":"20"},"rows":[{"object":{"metadata":{"name":"two"}}}]}`))
		},
	})
	defer cleanup()

	first := serve(p, http.MethodGet, "/api/v1/configmaps?limit=1", "")
	if first.Code != http.StatusOK || !contains(first.Body.String(), `"rows"`) || !contains(first.Body.String(), `"one"`) {
		t.Fatalf("first page = %d %s", first.Code, first.Body.String())
	}
	var firstPage struct {
		Metadata struct {
			Continue string `json:"continue"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal(first.Body.Bytes(), &firstPage); err != nil || firstPage.Metadata.Continue == "" {
		t.Fatalf("first page continuation = %q, err = %v", firstPage.Metadata.Continue, err)
	}
	second := serve(p, http.MethodGet, "/api/v1/configmaps?limit=1&continue="+firstPage.Metadata.Continue, "")
	if second.Code != http.StatusOK || !contains(second.Body.String(), `"two"`) || !contains(second.Body.String(), sourceContextAnnotation) {
		t.Fatalf("second page = %d %s", second.Code, second.Body.String())
	}
}

func TestAggregatePageRejectsExcessiveLimit(t *testing.T) {
	p, cleanup := newProxy(t, "one", map[string]http.HandlerFunc{
		"one": func(http.ResponseWriter, *http.Request) {},
		"two": func(http.ResponseWriter, *http.Request) {},
	})
	defer cleanup()
	recorder := serve(p, http.MethodGet, "/api/v1/configmaps?limit=10001", "")
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d: %s", recorder.Code, recorder.Body.String())
	}
}

func TestClassifyRequest(t *testing.T) {
	tests := []struct {
		name     string
		method   string
		path     string
		helmMode bool
		want     routeClass
	}{
		{name: "discovery", method: http.MethodGet, path: "/api", want: routePrimary},
		{name: "request response", method: http.MethodPost, path: "/apis/authorization.k8s.io/v1/selfsubjectaccessreviews", want: routePrimary},
		{name: "watch", method: http.MethodGet, path: "/api/v1/configmaps?watch=true", want: routeWatch},
		{name: "named watch", method: http.MethodGet, path: "/apis/apps/v1/namespaces/default/deployments/demo?watch=true", want: routeWatch},
		{name: "pod log", method: http.MethodGet, path: "/api/v1/namespaces/default/pods/demo/log", want: routePodStream},
		{name: "named object", method: http.MethodGet, path: "/api/v1/configmaps/demo", want: routeNamedGet},
		{name: "collection", method: http.MethodGet, path: "/api/v1/configmaps", want: routeList},
		{name: "mutation", method: http.MethodPost, path: "/api/v1/configmaps", want: routeMutation},
		{name: "helm list", method: http.MethodGet, path: "/api/v1/secrets?labelSelector=owner%3Dhelm", helmMode: true, want: routePrimary},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(test.method, test.path, http.NoBody)
			if got := classifyRequest(request, test.helmMode); got != test.want {
				t.Fatalf("classifyRequest() = %d, want %d", got, test.want)
			}
		})
	}
}

func TestAnnotationTargets(t *testing.T) {
	p, cleanup := newProxy(t, "two", map[string]http.HandlerFunc{
		"one": func(http.ResponseWriter, *http.Request) {},
		"two": func(http.ResponseWriter, *http.Request) {},
	})
	defer cleanup()

	targets, handled, err := p.annotationTargets(map[string]string{targetContextAnnotation: "one"}, false)
	if err != nil || !handled || len(targets) != 1 || targets[0].Name != "one" {
		t.Fatalf("target-context targets = %#v, handled = %t, err = %v", targets, handled, err)
	}
	targets, handled, err = p.annotationTargets(map[string]string{singleContextAnnotation: "true"}, false)
	if err != nil || !handled || len(targets) != 1 || targets[0].Name != "two" {
		t.Fatalf("single-context targets = %#v, handled = %t, err = %v", targets, handled, err)
	}
	targets, handled, err = p.annotationTargets(map[string]string{targetContextAnnotation: " one, two, one "}, false)
	if err != nil || !handled || len(targets) != 2 || targets[0].Name != "one" || targets[1].Name != "two" {
		t.Fatalf("target-context targets = %#v, handled = %t, err = %v", targets, handled, err)
	}
	_, handled, err = p.annotationTargets(map[string]string{targetContextAnnotation: "one,missing"}, false)
	if !handled || err == nil || !contains(err.Error(), "missing") {
		t.Fatalf("unknown target-context targets handled = %t, err = %v", handled, err)
	}
	_, handled, err = p.annotationTargets(map[string]string{targetContextAnnotation: ""}, false)
	if !handled || err == nil || !contains(err.Error(), "empty context name") {
		t.Fatalf("empty target-context targets handled = %t, err = %v", handled, err)
	}
	_, handled, err = p.annotationTargets(map[string]string{sourceContextAnnotation: "one,two"}, false)
	if handled || err != nil {
		t.Fatalf("source context handled = %t, err = %v", handled, err)
	}
	_, handled, err = p.annotationTargets(map[string]string{targetContextAnnotation: "missing"}, true)
	if !handled || err == nil || !contains(err.Error(), "existing object") {
		t.Fatalf("unknown target handled = %t, err = %v", handled, err)
	}
}

func TestListEntriesAndMarkEvent(t *testing.T) {
	entries, key, ok := listEntries(map[string]any{"rows": []any{map[string]any{}}})
	if !ok || key != "rows" || len(entries) != 1 {
		t.Fatalf("listEntries() = %#v, %q, %t", entries, key, ok)
	}
	if _, _, ok := listEntries(map[string]any{}); ok {
		t.Fatal("listEntries() accepted non-list payload")
	}
	event := markEvent([]byte(`{"type":"ADDED","object":{"metadata":{"name":"demo"}}}`), "two")
	if !contains(string(event), sourceContextAnnotation) || !contains(string(event), `"two"`) {
		t.Fatalf("event = %s", event)
	}
}

func TestMutationRoutingAndReadOnly(t *testing.T) {
	calls := &callLog{}
	p, cleanup := newProxy(t, "two", map[string]http.HandlerFunc{
		"one": calls.handler("one", `{"metadata":{"name":"demo"}}`),
		"two": calls.handler("two", `{"metadata":{"name":"demo"}}`),
	})
	defer cleanup()
	response := serve(p, http.MethodPost, "/api/v1/configmaps", `{"metadata":{"annotations":{"kubeconfig-proxy.io/target-context":"one"}}}`)
	if response.Code != http.StatusOK || len(calls.names()) != 1 || calls.names()[0] != "one" {
		t.Fatalf("target-context response = %d, calls = %v", response.Code, calls.names())
	}
	response = serve(p, http.MethodPost, "/api/v1/configmaps", `{"metadata":{"annotations":{"kubeconfig-proxy.io/target-context":"missing"}}}`)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("unknown context response = %d", response.Code)
	}
	calls = &callLog{}
	p, cleanup = newProxy(t, "two", map[string]http.HandlerFunc{
		"one": calls.handler("one", `{"metadata":{"name":"demo"}}`),
		"two": calls.handler("two", `{"metadata":{"name":"demo"}}`),
	})
	defer cleanup()
	response = serve(p, http.MethodPost, "/api/v1/configmaps", `{"metadata":{"annotations":{"kubeconfig-proxy.io/target-context":"one,two"}}}`)
	callsByContext := strings.Join(calls.names(), ",")
	if response.Code != http.StatusOK || strings.Count(callsByContext, "one") != 1 || strings.Count(callsByContext, "two") != 1 {
		t.Fatalf("context response = %d, calls = %v", response.Code, calls.names())
	}
	p.options.ReadOnly = true
	response = serve(p, http.MethodDelete, "/api/v1/configmaps/demo", "")
	if response.Code != http.StatusForbidden {
		t.Fatalf("read-only response = %d", response.Code)
	}
}

func TestMutationRemovesVirtualContextLabel(t *testing.T) {
	var mu sync.Mutex
	bodies := make([]string, 0, 2)
	handler := func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		mu.Lock()
		bodies = append(bodies, string(body))
		mu.Unlock()
		_, _ = w.Write([]byte(`{"metadata":{"name":"demo"}}`))
	}
	p, cleanup := newProxy(t, "one", map[string]http.HandlerFunc{
		"one": handler,
		"two": handler,
	})
	defer cleanup()

	response := serve(p, http.MethodPost, "/api/v1/configmaps", `{"metadata":{"labels":{"kcp-context":"one,two","keep":"value"}}}`)
	if response.Code != http.StatusOK || len(bodies) != 2 {
		t.Fatalf("response = %d, bodies = %v", response.Code, bodies)
	}
	for _, body := range bodies {
		if contains(body, `"kcp-context"`) || !contains(body, `"keep":"value"`) {
			t.Fatalf("forwarded body = %s", body)
		}
	}
}

func TestWithoutVirtualContextLabel(t *testing.T) {
	tests := []struct {
		name          string
		body          string
		wantUnchanged bool
	}{
		{name: "object", body: `{"metadata":{"labels":{"kcp-context":"one","keep":"value"}}}`},
		{name: "yaml object", body: "metadata:\n  labels:\n    kcp-context: one\n    keep: value\n"},
		{name: "without virtual label", body: `{"metadata":{"labels":{"keep":"value"}}}`, wantUnchanged: true},
		{name: "without metadata", body: `{}`, wantUnchanged: true},
		{name: "invalid body", body: "not: [valid", wantUnchanged: true},
		{name: "json patch", body: `[{"op":"add","path":"/metadata/labels/kcp-context","value":"one"},{"op":"add","path":"/metadata/labels/keep","value":"value"}]`, wantUnchanged: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			body := withoutVirtualContextLabel([]byte(test.body))
			if test.wantUnchanged {
				if string(body) != test.body {
					t.Fatalf("body = %s", body)
				}
				return
			}
			if contains(string(body), "kcp-context") {
				t.Fatalf("body = %s", body)
			}
		})
	}
}

func TestMarkNamedGetBody(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{
			name: "object",
			body: `{"metadata":{"labels":{"keep":"value"}}}`,
			want: `"kcp-context":"one,two"`,
		},
		{
			name: "table row",
			body: `{"rows":[{"object":{"metadata":{"labels":{"keep":"value"}}}}]}`,
			want: `"kcp-context":"one,two"`,
		},
		{
			name: "invalid JSON",
			body: "not JSON",
			want: "not JSON",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			body := markNamedGetBody([]byte(test.body), "one,two")
			if !contains(string(body), test.want) {
				t.Fatalf("body = %s, want %q", body, test.want)
			}
		})
	}
}

func TestMarkContextLabel(t *testing.T) {
	entry := map[string]any{}
	markContextLabel(entry, "one")
	metadata, ok := entry["metadata"].(map[string]any)
	if !ok {
		t.Fatalf("metadata = %#v", entry["metadata"])
	}
	labels, ok := metadata["labels"].(map[string]any)
	if !ok || labels[sourceContextLabel] != "one" {
		t.Fatalf("labels = %#v", metadata["labels"])
	}

	markContextLabel("not an object", "one")
	markEntry("not an object", "one")
}

func TestPatchPreservesVirtualContextLabel(t *testing.T) {
	var mu sync.Mutex
	bodies := make([]string, 0, 2)
	handler := func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		mu.Lock()
		bodies = append(bodies, string(body))
		mu.Unlock()
		_, _ = w.Write([]byte(`{"metadata":{"name":"demo"}}`))
	}
	p, cleanup := newProxy(t, "one", map[string]http.HandlerFunc{
		"one": handler,
		"two": handler,
	})
	defer cleanup()

	patch := `{"metadata":{"labels":{"kcp-context":"one,two"}}}`
	response := serve(p, http.MethodPatch, "/api/v1/configmaps", patch)
	if response.Code != http.StatusOK || len(bodies) != 2 {
		t.Fatalf("response = %d, bodies = %v", response.Code, bodies)
	}
	for _, body := range bodies {
		if body != patch {
			t.Fatalf("forwarded body = %s", body)
		}
	}
}

func TestAggregateHelpers(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "/api/v1/configmaps?labelSelector=x&limit=3", http.NoBody)
	if limit, err := aggregatePageLimit(request); err != nil || limit != 3 {
		t.Fatalf("aggregatePageLimit() = %d, %v", limit, err)
	}
	if pageScope(request) != "/api/v1/configmaps?labelSelector=x" {
		t.Fatalf("pageScope() = %q", pageScope(request))
	}
	payload := map[string]any{"items": []any{}}
	setCursor(payload, pageCursor{Target: 1, Scope: pageScope(request)})
	if !contains(payload["metadata"].(map[string]any)["continue"].(string), aggregateContinuePrefix) {
		t.Fatal("setCursor() did not create aggregate token")
	}
	merged, err := mergeLists([]upstreamResponse{
		{target: Target{Name: "one"}, body: []byte(`{"items":[{"metadata":{"name":"one"}}]}`)},
		{target: Target{Name: "two"}, body: []byte(`{"items":[{"metadata":{"name":"two"}}]}`)},
	})
	if err != nil || !contains(string(merged), `"two"`) || !contains(string(merged), `"kcp-context"`) {
		t.Fatalf("mergeLists() = %s, %v", merged, err)
	}
}

func TestAggregateCursorRejectsChangedTargetSet(t *testing.T) {
	p, cleanup := newProxy(t, "one", map[string]http.HandlerFunc{
		"one": func(http.ResponseWriter, *http.Request) {},
		"two": func(http.ResponseWriter, *http.Request) {},
	})
	defer cleanup()

	request := httptest.NewRequest(http.MethodGet, "/api/v1/configmaps?limit=1", http.NoBody)
	payload := map[string]any{"items": []any{}}
	setCursor(payload, pageCursor{Target: 0, Continue: "context-one-token", Scope: pageScope(request)})
	continueToken := payload["metadata"].(map[string]any)["continue"].(string)

	p.targets[0].Name = "replacement"
	continued := httptest.NewRequest(http.MethodGet, "/api/v1/configmaps?limit=1&continue="+url.QueryEscape(continueToken), http.NoBody)
	if _, err := p.decodeCursor(continued); err == nil {
		t.Fatal("decodeCursor accepted a token from a different configured target set")
	}
}

func TestResponseHelpers(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "/", http.NoBody)
	request.Header.Set("Authorization", "Bearer token")
	if !AuthorizedWithToken(request, "token") || AuthorizedWithToken(request, "other") || AuthorizedWithToken(httptest.NewRequest(http.MethodGet, "/", http.NoBody), "token") {
		t.Fatal("authorization token comparison is incorrect")
	}
	if !isHopHeader("Connection") || isHopHeader("Content-Type") {
		t.Fatal("hop header classification is incorrect")
	}
}

func TestAggregateWatchForwardsAndMarksEvents(t *testing.T) {
	p, cleanup := newProxy(t, "one", map[string]http.HandlerFunc{
		"one": func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte("{\"type\":\"ADDED\",\"object\":{\"metadata\":{\"name\":\"one\"}}}\n"))
		},
		"two": func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte("{\"type\":\"ADDED\",\"object\":{\"metadata\":{\"name\":\"two\"}}}\n"))
		},
	})
	defer cleanup()
	response := serve(p, http.MethodGet, "/api/v1/configmaps?watch=true", "")
	if response.Code != http.StatusOK || !contains(response.Body.String(), `"one"`) || !contains(response.Body.String(), `"two"`) || !contains(response.Body.String(), sourceContextAnnotation) {
		t.Fatalf("watch response = %d %s", response.Code, response.Body.String())
	}
}

func TestAggregateWatchDecodesNamedDeploymentEventsWithDynamicClient(t *testing.T) {
	watchPaths := make(chan string, 2)
	p, cleanup := newProxy(t, "one", map[string]http.HandlerFunc{
		"one": func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Query().Get("watch") != "true" {
				_, _ = w.Write([]byte(`{"apiVersion":"apps/v1","kind":"Deployment","metadata":{"name":"demo","resourceVersion":"10"}}`))
				return
			}
			watchPaths <- r.URL.Path + "?" + r.URL.RawQuery
			w.Header().Set("Content-Type", "application/json;stream=watch")
			_, _ = w.Write([]byte(`{"type":"MODIFIED","object":{"apiVersion":"apps/v1","kind":"Deployment","metadata":{"name":"demo"}}}` + "\n"))
		},
		"two": func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Query().Get("watch") != "true" {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			watchPaths <- r.URL.Path + "?" + r.URL.RawQuery
			w.Header().Set("Content-Type", "application/json;stream=watch")
			_, _ = w.Write([]byte(`{"type":"ADDED","object":{"apiVersion":"apps/v1","kind":"Deployment","metadata":{"name":"demo"}}}` + "\n"))
		},
	})
	defer cleanup()

	proxyServer := httptest.NewServer(p)
	defer proxyServer.Close()
	client, err := dynamic.NewForConfig(&rest.Config{Host: proxyServer.URL, BearerToken: "test"})
	if err != nil {
		t.Fatal(err)
	}
	watcher, err := client.Resource(schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}).Namespace("default").Watch(context.Background(), metav1.ListOptions{FieldSelector: "metadata.name=demo"})
	if err != nil {
		t.Fatal(err)
	}
	defer watcher.Stop()

	event := <-watcher.ResultChan()
	if event.Type != "ADDED" || event.Object == nil {
		t.Fatalf("event = %#v", event)
	}
	object, ok := event.Object.(*unstructured.Unstructured)
	if !ok || object.GetAnnotations()[sourceContextAnnotation] != "one" {
		t.Fatalf("initial event object = %#v", event.Object)
	}
	select {
	case watchPath := <-watchPaths:
		if got, want := watchPath, "/apis/apps/v1/namespaces/default/deployments?fieldSelector=metadata.name%3Ddemo&resourceVersion=10&watch=true"; got != want {
			t.Fatalf("watch path = %q, want %q", got, want)
		}
	default:
		t.Fatal("watch request was not sent")
	}
}

func TestNamedWatchOpensAllContexts(t *testing.T) {
	watches := &callLog{}
	var mu sync.Mutex
	watchResourceVersions := map[string]string{}
	namedWatchHandler := func(name, resourceVersion string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Query().Get("watch") != "true" {
				_, _ = w.Write([]byte(`{"metadata":{"name":"demo","resourceVersion":"` + resourceVersion + `"}}`))
				return
			}
			mu.Lock()
			watchResourceVersions[name] = r.URL.Query().Get("resourceVersion")
			mu.Unlock()
			watches.mu.Lock()
			watches.values = append(watches.values, name)
			watches.mu.Unlock()
			_, _ = w.Write([]byte(`{"type":"MODIFIED","object":{"metadata":{"name":"demo"}}}` + "\n"))
		}
	}
	p, cleanup := newProxy(t, "one", map[string]http.HandlerFunc{
		"one": namedWatchHandler("one", "10"),
		"two": namedWatchHandler("two", "20"),
	})
	defer cleanup()

	response := serve(p, http.MethodGet, "/apis/apps/v1/namespaces/default/deployments/demo?watch=true&resourceVersion=10", "")
	if response.Code != http.StatusOK {
		t.Fatalf("response = %d %s", response.Code, response.Body.String())
	}
	if strings.Contains(response.Body.String(), `"type":"ADDED"`) {
		t.Fatalf("watch response contains an unexpected initial event: %s", response.Body.String())
	}
	if got := watches.names(); len(got) != 2 || !contains(strings.Join(got, ","), "one") || !contains(strings.Join(got, ","), "two") {
		t.Fatalf("watch calls = %v, want both contexts", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if got, want := watchResourceVersions["one"], "10"; got != want {
		t.Fatalf("one resourceVersion = %q, want %q", got, want)
	}
	if got, want := watchResourceVersions["two"], "20"; got != want {
		t.Fatalf("two resourceVersion = %q, want %q", got, want)
	}
}

func TestNamedWatchSkipsAbsentContexts(t *testing.T) {
	watches := &callLog{}
	namedWatchHandler := func(name string, exists bool) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Query().Get("watch") != "true" {
				if !exists {
					w.WriteHeader(http.StatusNotFound)
					return
				}
				_, _ = w.Write([]byte(`{"metadata":{"name":"demo","resourceVersion":"10"}}`))
				return
			}
			watches.mu.Lock()
			watches.values = append(watches.values, name)
			watches.mu.Unlock()
			_, _ = w.Write([]byte(`{"type":"MODIFIED","object":{"metadata":{"name":"demo"}}}` + "\n"))
		}
	}
	p, cleanup := newProxy(t, "one", map[string]http.HandlerFunc{
		"one": namedWatchHandler("one", true),
		"two": namedWatchHandler("two", false),
	})
	defer cleanup()

	response := serve(p, http.MethodGet, "/apis/apps/v1/namespaces/default/deployments/demo?watch=true", "")
	if response.Code != http.StatusOK {
		t.Fatalf("response = %d %s", response.Code, response.Body.String())
	}
	if got := watches.names(); len(got) != 1 || got[0] != "one" {
		t.Fatalf("watch calls = %v, want [one]", got)
	}
}

func TestPodStreamUsesOwningTarget(t *testing.T) {
	p, cleanup := newProxy(t, "one", map[string]http.HandlerFunc{
		"one": func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/api/v1/namespaces/default/pods/demo" {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			_, _ = w.Write([]byte("wrong"))
		},
		"two": func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/api/v1/namespaces/default/pods/demo" {
				_, _ = w.Write([]byte(`{"metadata":{"name":"demo"}}`))
				return
			}
			_, _ = w.Write([]byte("pod logs"))
		},
	})
	defer cleanup()
	response := serve(p, http.MethodGet, "/api/v1/namespaces/default/pods/demo/log", "")
	if response.Code != http.StatusOK || response.Body.String() != "pod logs" {
		t.Fatalf("pod stream = %d %q", response.Code, response.Body.String())
	}
}

func TestPutUsesExistingObjectIdentity(t *testing.T) {
	var putBody string
	p, cleanup := newProxy(t, "one", map[string]http.HandlerFunc{
		"one": func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodGet {
				_, _ = w.Write([]byte(`{"metadata":{"uid":"uid-one","resourceVersion":"7"}}`))
				return
			}
			data, _ := readBody(r.Body, maxBodyBytes)
			putBody = string(data)
			_, _ = w.Write([]byte(`{"metadata":{"name":"demo"}}`))
		},
		"two": func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) },
	})
	defer cleanup()
	response := serve(p, http.MethodPut, "/api/v1/configmaps/demo", `{"metadata":{"name":"demo"}}`)
	if response.Code != http.StatusOK || !contains(putBody, `"uid":"uid-one"`) || !contains(putBody, `"resourceVersion":"7"`) {
		t.Fatalf("PUT response = %d body = %s", response.Code, putBody)
	}
}

func TestAggregateAndWatchFailures(t *testing.T) {
	t.Run("invalid cursor", func(t *testing.T) {
		p, cleanup := newProxy(t, "one", map[string]http.HandlerFunc{"one": func(http.ResponseWriter, *http.Request) {}, "two": func(http.ResponseWriter, *http.Request) {}})
		defer cleanup()
		response := serve(p, http.MethodGet, "/api/v1/configmaps?limit=1&continue=foreign", "")
		if response.Code != http.StatusBadRequest {
			t.Fatalf("status = %d", response.Code)
		}
	})
	t.Run("invalid list", func(t *testing.T) {
		p, cleanup := newProxy(t, "one", map[string]http.HandlerFunc{"one": func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{}`)) }, "two": func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{}`)) }})
		defer cleanup()
		response := serve(p, http.MethodGet, "/api/v1/configmaps", "")
		if response.Code != http.StatusBadGateway {
			t.Fatalf("status = %d", response.Code)
		}
	})
	t.Run("watch open failure", func(t *testing.T) {
		p, cleanup := newProxy(t, "one", map[string]http.HandlerFunc{"one": func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) }, "two": func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("{}\n")) }})
		defer cleanup()
		response := serve(p, http.MethodGet, "/api/v1/configmaps?watch=true", "")
		if response.Code != http.StatusServiceUnavailable || !contains(response.Body.String(), "one") {
			t.Fatalf("response = %d %s", response.Code, response.Body.String())
		}
	})
}

func TestAggregateWatchClosesStreamsOpenedAfterFailure(t *testing.T) {
	openedStream := make(chan struct{})
	streamClosed := make(chan struct{})
	failureHost, err := url.Parse("https://failure.example.test")
	if err != nil {
		t.Fatal(err)
	}
	streamHost, err := url.Parse("https://stream.example.test")
	if err != nil {
		t.Fatal(err)
	}
	targets := []Target{
		{
			Name: "failure",
			Host: failureHost,
			Client: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				<-openedStream
				return &http.Response{StatusCode: http.StatusServiceUnavailable, Body: io.NopCloser(strings.NewReader("unavailable")), Header: make(http.Header)}, nil
			})},
		},
		{
			Name: "stream",
			Host: streamHost,
			Client: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				close(openedStream)
				return &http.Response{StatusCode: http.StatusOK, Body: &closeTrackingBody{Reader: strings.NewReader("event\n"), closed: streamClosed}, Header: make(http.Header)}, nil
			})},
		},
	}
	p, err := NewWithOptions(targets, targets[0], Options{BearerToken: "test"})
	if err != nil {
		t.Fatal(err)
	}

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/api/v1/configmaps?watch=true", http.NoBody)
	p.aggregateWatch(recorder, request)
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusServiceUnavailable)
	}
	select {
	case <-streamClosed:
	default:
		t.Fatal("stream opened after another target failed was not closed")
	}
}

func TestAggregateListWithoutPageAndNamedFallbacks(t *testing.T) {
	p, cleanup := newProxy(t, "one", map[string]http.HandlerFunc{
		"one": func(w http.ResponseWriter, r *http.Request) {
			if strings.Contains(r.URL.RawQuery, "limit") {
				t.Errorf("limit was forwarded: %s", r.URL.RawQuery)
			}
			if strings.HasSuffix(r.URL.Path, "/demo") {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			_, _ = w.Write([]byte(`{"items":[{"metadata":{"name":"one"}}]}`))
		},
		"two": func(w http.ResponseWriter, r *http.Request) {
			if strings.HasSuffix(r.URL.Path, "/demo") {
				_, _ = w.Write([]byte(`{"metadata":{"name":"demo"}}`))
				return
			}
			_, _ = w.Write([]byte(`{"items":[{"metadata":{"name":"two"}}]}`))
		},
	})
	defer cleanup()
	list := serve(p, http.MethodGet, "/api/v1/configmaps?limit=0", "")
	if list.Code != http.StatusOK || !contains(list.Body.String(), `"two"`) {
		t.Fatalf("list = %d %s", list.Code, list.Body.String())
	}
	named := serve(p, http.MethodGet, "/api/v1/configmaps/demo", "")
	if named.Code != http.StatusOK || !contains(named.Body.String(), `"demo"`) {
		t.Fatalf("named = %d %s", named.Code, named.Body.String())
	}
}

func TestNamedAndPodFallbackToPrimary(t *testing.T) {
	p, cleanup := newProxy(t, "one", map[string]http.HandlerFunc{
		"one": func(w http.ResponseWriter, r *http.Request) {
			if strings.HasSuffix(r.URL.Path, "/demo") {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			_, _ = w.Write([]byte("primary"))
		},
		"two": func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) },
	})
	defer cleanup()
	if got := serve(p, http.MethodGet, "/api/v1/configmaps/demo", ""); got.Code != http.StatusNotFound {
		t.Fatalf("named = %d", got.Code)
	}
	if got := serve(p, http.MethodGet, "/api/v1/namespaces/default/pods/demo/log", ""); got.Code != http.StatusOK || got.Body.String() != "primary" {
		t.Fatalf("pod = %d %s", got.Code, got.Body.String())
	}
}

func TestTransportRetriesRetryableResponse(t *testing.T) {
	attempts := 0
	p, cleanup := newProxy(t, "one", map[string]http.HandlerFunc{
		"one": func(w http.ResponseWriter, _ *http.Request) {
			attempts++
			if attempts == 1 {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			_, _ = w.Write([]byte(`{"items":[]}`))
		},
		"two": func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{"items":[]}`)) },
	})
	defer cleanup()
	p.options.Retries = 1
	response := serve(p, http.MethodGet, "/api/v1/configmaps", "")
	if response.Code != http.StatusOK || attempts != 2 {
		t.Fatalf("response = %d, attempts = %d", response.Code, attempts)
	}
}

func TestMutationExistingObjectErrors(t *testing.T) {
	t.Run("non-404 blocks mutation", func(t *testing.T) {
		p, cleanup := newProxy(t, "one", map[string]http.HandlerFunc{
			"one": func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet {
					w.WriteHeader(http.StatusForbidden)
					return
				}
				t.Fatal("mutation must not be sent")
			},
			"two": func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) },
		})
		defer cleanup()
		response := serve(p, http.MethodPatch, "/api/v1/configmaps/demo", `{}`)
		if response.Code != http.StatusBadRequest || !contains(response.Body.String(), "HTTP 403") {
			t.Fatalf("response = %d %s", response.Code, response.Body.String())
		}
	})
	t.Run("existing target-context annotation selects target", func(t *testing.T) {
		calls := &callLog{}
		p, cleanup := newProxy(t, "one", map[string]http.HandlerFunc{
			"one": calls.handler("one", `{"metadata":{"annotations":{"kubeconfig-proxy.io/target-context":"two"}}}`),
			"two": calls.handler("two", `{"metadata":{"name":"demo"}}`),
		})
		defer cleanup()
		response := serve(p, http.MethodPatch, "/api/v1/configmaps/demo", `{}`)
		callsByContext := calls.names()
		if response.Code != http.StatusOK || len(callsByContext) != 3 || strings.Count(strings.Join(callsByContext, ","), "two") != 2 {
			t.Fatalf("response = %d calls = %v", response.Code, calls.names())
		}
	})
	t.Run("existing target-context annotation selects targets", func(t *testing.T) {
		calls := &callLog{}
		p, cleanup := newProxy(t, "one", map[string]http.HandlerFunc{
			"one": calls.handler("one", `{"metadata":{"annotations":{"kubeconfig-proxy.io/target-context":"one,two"}}}`),
			"two": calls.handler("two", `{"metadata":{"name":"demo"}}`),
		})
		defer cleanup()
		response := serve(p, http.MethodPatch, "/api/v1/configmaps/demo", `{}`)
		callsByContext := strings.Join(calls.names(), ",")
		if response.Code != http.StatusOK || strings.Count(callsByContext, "one") != 2 || strings.Count(callsByContext, "two") != 2 {
			t.Fatalf("response = %d calls = %v", response.Code, calls.names())
		}
	})
}

type callLog struct {
	mu     sync.Mutex
	values []string
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

type closeTrackingBody struct {
	io.Reader
	closed chan<- struct{}
	once   sync.Once
}

func (b *closeTrackingBody) Close() error {
	b.once.Do(func() { close(b.closed) })
	return nil
}

func (c *callLog) handler(name, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		c.mu.Lock()
		c.values = append(c.values, name)
		c.mu.Unlock()
		_, _ = w.Write([]byte(body))
	}
}
func (c *callLog) names() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.values...)
}

func newProxy(t *testing.T, primary string, handlers map[string]http.HandlerFunc) (*Proxy, func()) {
	t.Helper()
	targets := make([]Target, 0, len(handlers))
	servers := make([]*httptest.Server, 0, len(handlers))
	for _, name := range []string{"one", "two"} {
		server := httptest.NewServer(handlers[name])
		servers = append(servers, server)
		host, err := url.Parse(server.URL)
		if err != nil {
			t.Fatal(err)
		}
		targets = append(targets, Target{Name: name, Host: host, Client: server.Client()})
	}
	var primaryTarget Target
	for _, target := range targets {
		if target.Name == primary {
			primaryTarget = target
		}
	}
	proxy, err := NewWithOptions(targets, primaryTarget, Options{BearerToken: "test"})
	if err != nil {
		t.Fatal(err)
	}
	return proxy, func() {
		for _, server := range servers {
			server.Close()
		}
	}
}

func serve(p *Proxy, method, path, body string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, path, stringReader(body))
	request.Header.Set("Authorization", "Bearer test")
	recorder := httptest.NewRecorder()
	p.ServeHTTP(recorder, request)
	return recorder
}
func stringReader(value string) *strings.Reader { return strings.NewReader(value) }
func contains(value, part string) bool          { return strings.Contains(value, part) }
