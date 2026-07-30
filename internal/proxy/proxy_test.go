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
