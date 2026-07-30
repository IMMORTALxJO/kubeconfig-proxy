package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
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

func TestNamedGetPrefersPrimary(t *testing.T) {
	calls := &callLog{}
	p, cleanup := newProxy(t, "two", map[string]http.HandlerFunc{
		"one": calls.handler("one", `{"metadata":{"labels":{"context":"one"}}}`),
		"two": calls.handler("two", `{"metadata":{"labels":{"context":"two"}}}`),
	})
	defer cleanup()
	recorder := serve(p, http.MethodGet, "/api/v1/namespaces/default/configmaps/demo", "")
	if recorder.Code != http.StatusOK || !contains(recorder.Body.String(), `"two"`) {
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
	if recorder.Code != http.StatusOK || !contains(recorder.Body.String(), `kubeconfig-proxy.io/context`) {
		t.Fatalf("response = %d %s", recorder.Code, recorder.Body.String())
	}
}

func TestAggregatePageSupportsTableResponses(t *testing.T) {
	p, cleanup := newProxy(t, "one", map[string]http.HandlerFunc{
		"one": func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"kind":"Table","metadata":{},"rows":[{"object":{"metadata":{"name":"one"}}}]}`))
		},
		"two": func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"kind":"Table","metadata":{},"rows":[{"object":{"metadata":{"name":"two"}}}]}`))
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
		{name: "pod log", method: http.MethodGet, path: "/api/v1/namespaces/default/pods/demo/log", want: routePodStream},
		{name: "named object", method: http.MethodGet, path: "/api/v1/configmaps/demo", want: routeNamedGet},
		{name: "collection", method: http.MethodGet, path: "/api/v1/configmaps", want: routeList},
		{name: "mutation", method: http.MethodPost, path: "/api/v1/configmaps", want: routeMutation},
		{name: "helm list", method: http.MethodGet, path: "/api/v1/secrets?labelSelector=owner%3Dhelm", helmMode: true, want: routePrimary},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(test.method, test.path, nil)
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

	targets, handled, err := p.annotationTargets(map[string]string{contextNameAnnotation: "one"}, false)
	if err != nil || !handled || len(targets) != 1 || targets[0].Name != "one" {
		t.Fatalf("context-name targets = %#v, handled = %t, err = %v", targets, handled, err)
	}
	targets, handled, err = p.annotationTargets(map[string]string{singleContextAnnotation: "true"}, false)
	if err != nil || !handled || len(targets) != 1 || targets[0].Name != "two" {
		t.Fatalf("single-context targets = %#v, handled = %t, err = %v", targets, handled, err)
	}
	_, handled, err = p.annotationTargets(map[string]string{contextNameAnnotation: "missing"}, true)
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

type callLog struct {
	mu     sync.Mutex
	values []string
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
