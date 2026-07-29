package proxy

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
)

func TestParseListLimit(t *testing.T) {
	tests := []struct {
		value   string
		want    int
		wantErr bool
	}{
		{value: "", want: 0},
		{value: "0", want: 0},
		{value: "25", want: 25},
		{value: "-1", wantErr: true},
		{value: "invalid", wantErr: true},
		{value: "2147483648", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.value, func(t *testing.T) {
			got, err := parseListLimit(tt.value)
			if (err != nil) != tt.wantErr {
				t.Fatalf("parseListLimit(%q) error = %v, wantErr %v", tt.value, err, tt.wantErr)
			}
			if got != tt.want {
				t.Fatalf("parseListLimit(%q) = %d, want %d", tt.value, got, tt.want)
			}
		})
	}
}

func TestDecodeListCursor(t *testing.T) {
	targets := []string{"one", "two"}
	scope := "/api/v1/pods?labelSelector=app%3Ddemo"
	resourceVersions := map[string]string{"one": "10"}
	token := encodeListCursor(targets, "two", "offset-1", resourceVersions, scope)

	index, upstreamContinue, gotVersions, err := decodeListCursor(token, targets, scope)
	if err != nil {
		t.Fatal(err)
	}
	if index != 1 || upstreamContinue != "offset-1" {
		t.Fatalf("decoded cursor = index %d, continue %q", index, upstreamContinue)
	}
	if gotVersions["one"] != "10" {
		t.Fatalf("resource versions = %v, want one=10", gotVersions)
	}
	gotVersions["one"] = "changed"
	if resourceVersions["one"] != "10" {
		t.Fatal("decoded resource versions alias the source map")
	}

	index, upstreamContinue, gotVersions, err = decodeListCursor("", targets, scope)
	if err != nil || index != 0 || upstreamContinue != "" || len(gotVersions) != 0 {
		t.Fatalf("empty cursor = %d, %q, %v, %v", index, upstreamContinue, gotVersions, err)
	}
}

func TestDecodeListCursorRejectsInvalidTokens(t *testing.T) {
	targets := []string{"one"}
	scope := "/api/v1/pods?"
	invalidJSON := aggregateContinuePrefix + base64.RawURLEncoding.EncodeToString([]byte("{"))
	tests := []struct {
		name  string
		token string
		want  string
	}{
		{name: "not aggregate", token: "upstream-token", want: "invalid aggregate continue token"},
		{name: "invalid base64", token: aggregateContinuePrefix + "!", want: "decode aggregate continue token"},
		{name: "invalid JSON", token: invalidJSON, want: "decode aggregate continue token"},
		{name: "different targets", token: encodeListCursor([]string{"two"}, "two", "", nil, scope), want: "does not match configured targets"},
		{name: "different request", token: encodeListCursor(targets, "one", "", nil, "/api/v1/services?"), want: "does not match request"},
		{name: "missing target", token: encodeListCursor(targets, "two", "", nil, scope), want: `target "two" is not configured`},
		{name: "unknown resource version target", token: encodeListCursor(targets, "one", "", map[string]string{"two": "20"}, scope), want: `resource version target "two" is not configured`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, _, err := decodeListCursor(tt.token, targets, scope)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want to contain %q", err, tt.want)
			}
		})
	}
}

func TestPaginationRequestHelpers(t *testing.T) {
	original := httptest.NewRequest(http.MethodGet, "/api/v1/pods?limit=10&continue=aggregate&labelSelector=app%3Ddemo", http.NoBody)
	if got := scopeForListCursor(original); got != "/api/v1/pods?labelSelector=app%3Ddemo" {
		t.Fatalf("cursor scope = %q", got)
	}

	targetRequest := paginatedRequestForTarget(original, 3, "upstream")
	if got := targetRequest.URL.Query().Get("limit"); got != "3" {
		t.Fatalf("target limit = %q, want 3", got)
	}
	if got := targetRequest.URL.Query().Get("continue"); got != "upstream" {
		t.Fatalf("target continue = %q, want upstream", got)
	}

	unlimitedRequest := paginatedRequestForTarget(original, 0, "")
	if _, ok := unlimitedRequest.URL.Query()["limit"]; ok {
		t.Fatal("unlimited request kept limit")
	}
	if _, ok := unlimitedRequest.URL.Query()["continue"]; ok {
		t.Fatal("request without upstream cursor kept continue")
	}
	if original.URL.Query().Get("limit") != "10" {
		t.Fatal("paginated request mutated the original request")
	}
}

func TestInspectListPage(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		want    listPageInfo
		wantErr string
	}{
		{name: "items", body: `{"metadata":{"resourceVersion":"10","continue":"next"},"items":[{},{}]}`, want: listPageInfo{arrayKey: "items", itemCount: 2, continueToken: "next", resourceVersion: "10"}},
		{name: "table rows", body: `{"metadata":{"resourceVersion":"20"},"rows":[{}]}`, want: listPageInfo{arrayKey: "rows", itemCount: 1, resourceVersion: "20"}},
		{name: "invalid JSON", body: "{", wantErr: "decode list response"},
		{name: "missing array", body: `{"metadata":{}}`, wantErr: "not a Kubernetes list or table"},
		{name: "non-array items", body: `{"items":{}}`, wantErr: "not a Kubernetes list or table"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := inspectListPage([]byte(tt.body))
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %v, want to contain %q", err, tt.wantErr)
				}
				return
			}
			if got != tt.want {
				t.Fatalf("page = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestSetAggregateContinue(t *testing.T) {
	body := []byte(`{"metadata":{"remainingItemCount":3},"items":[]}`)
	withContinue, err := setAggregateContinue(body, "next")
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(withContinue, &payload); err != nil {
		t.Fatal(err)
	}
	metadata := payload["metadata"].(map[string]any)
	if metadata["continue"] != "next" {
		t.Fatalf("continue = %v, want next", metadata["continue"])
	}
	if _, ok := metadata["remainingItemCount"]; ok {
		t.Fatal("remainingItemCount was not removed")
	}

	withoutContinue, err := setAggregateContinue(withContinue, "")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(withoutContinue), `"continue"`) {
		t.Fatalf("empty continue was retained: %s", withoutContinue)
	}
	if _, err := setAggregateContinue([]byte("{"), "next"); err == nil {
		t.Fatal("invalid merged list returned nil error")
	}
}

func TestPaginatedListHandlesUpstreamFailures(t *testing.T) {
	t.Run("non-success response", func(t *testing.T) {
		assertPaginatedProxyResponse(t, map[string]http.HandlerFunc{
			"one": func(w http.ResponseWriter, _ *http.Request) {
				http.Error(w, "forbidden", http.StatusForbidden)
			},
		}, "/api/v1/pods?limit=1", http.StatusForbidden, "forbidden")
	})
	t.Run("invalid list", func(t *testing.T) {
		assertPaginatedProxyResponse(t, map[string]http.HandlerFunc{
			"one": func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte("{"))
			},
		}, "/api/v1/pods?limit=1", http.StatusBadGateway, "decode list response")
	})
	t.Run("different list fields", func(t *testing.T) {
		assertPaginatedProxyResponse(t, map[string]http.HandlerFunc{
			"one": func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(`{"items":[]}`))
			},
			"two": func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(`{"rows":[]}`))
			},
		}, "/api/v1/pods?limit=1", http.StatusBadGateway, "does not match")
	})
	t.Run("upstream exceeds remaining limit", func(t *testing.T) {
		assertPaginatedProxyResponse(t, map[string]http.HandlerFunc{
			"one": func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(`{"items":[{},{}]}`))
			},
		}, "/api/v1/pods?limit=1", http.StatusBadGateway, "upstream returned 2 items")
	})
	t.Run("invalid limit", func(t *testing.T) {
		assertPaginatedProxyResponse(t, map[string]http.HandlerFunc{
			"one": func(http.ResponseWriter, *http.Request) {
				t.Fatal("upstream was called for an invalid limit")
			},
		}, "/api/v1/pods?limit=invalid", http.StatusBadRequest, "invalid list limit")
	})
}

func TestPaginatedListHandlesTransportFailure(t *testing.T) {
	upstreamErr := errors.New("connection failed")
	target := Target{
		Name: "one",
		Host: &url.URL{Scheme: "https", Host: "one.example.test"},
		Client: &http.Client{Transport: roundTripperFunc(func(*http.Request) (*http.Response, error) {
			return nil, upstreamErr
		})},
	}
	p, err := newTestProxy([]Target{target}, target)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/pods?limit=1", http.NoBody)
	rec := httptest.NewRecorder()
	serveTestHTTP(p, rec, req)
	if rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), upstreamErr.Error()) {
		t.Fatalf("response = %d %s, want transport error", rec.Code, rec.Body.String())
	}
}

func TestPaginatedTableResponse(t *testing.T) {
	assertPaginatedProxyResponse(t, map[string]http.HandlerFunc{
		"one": func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"kind":"Table","metadata":{"remainingItemCount":1},"rows":[{"object":{"metadata":{"name":"pod-a"}}}]}`))
		},
	}, "/api/v1/pods?limit=1", http.StatusOK, `"rows"`)
}

func TestAggregatedPaginationAcceptsClientGoContinuationWithoutResourceVersion(t *testing.T) {
	targets, cleanup := testTargets(t, map[string]http.HandlerFunc{
		"one": paginatedListHandler(t, "10", []string{"a1", "a2"}),
	})
	defer cleanup()
	p, err := newTestProxy(targets, targets[0])
	if err != nil {
		t.Fatal(err)
	}

	firstQuery := url.Values{
		"limit":                []string{"1"},
		"resourceVersion":      []string{"0"},
		"resourceVersionMatch": []string{"NotOlderThan"},
	}
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

	nextQuery := url.Values{
		"limit":    []string{"1"},
		"continue": []string{firstPage.Metadata.Continue},
	}
	nextReq := httptest.NewRequest(http.MethodGet, "/api/v1/pods?"+nextQuery.Encode(), http.NoBody)
	nextRec := httptest.NewRecorder()
	serveTestHTTP(p, nextRec, nextReq)
	if nextRec.Code != http.StatusOK {
		t.Fatalf("continued page status = %d, want %d; body=%s", nextRec.Code, http.StatusOK, nextRec.Body.String())
	}
}

func TestPaginatedListFirstPageIncludesResourceVersionsForAllTargets(t *testing.T) {
	targets, cleanup := testTargets(t, map[string]http.HandlerFunc{
		"one": paginatedListHandler(t, "10", []string{"a1", "a2"}),
		"two": paginatedListHandler(t, "20", []string{"b1"}),
	})
	defer cleanup()
	p, err := newTestProxy(targets, targets[0])
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/pods?limit=1", http.NoBody)
	rec := httptest.NewRecorder()
	serveTestHTTP(p, rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("first page status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}

	var page struct {
		Metadata struct {
			ResourceVersion string `json:"resourceVersion"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	resourceVersions, ok := decodeAggregateResourceVersion(page.Metadata.ResourceVersion)
	if !ok {
		t.Fatalf("first page resourceVersion = %q, want aggregate resource version", page.Metadata.ResourceVersion)
	}
	if want := map[string]string{"one": "10", "two": "20"}; !mapsEqual(resourceVersions, want) {
		t.Fatalf("first page resourceVersions = %v, want %v", resourceVersions, want)
	}
}

func assertPaginatedProxyResponse(t *testing.T, handlers map[string]http.HandlerFunc, rawURL string, wantStatus int, wantBody string) {
	t.Helper()
	targets, cleanup := testTargets(t, handlers)
	defer cleanup()
	p, err := newTestProxy(targets, targets[0])
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, rawURL, http.NoBody)
	rec := httptest.NewRecorder()
	serveTestHTTP(p, rec, req)
	if rec.Code != wantStatus {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, wantStatus, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), wantBody) {
		t.Fatalf("body = %s, want to contain %q", rec.Body.String(), wantBody)
	}
}

func TestCursorTargetNamesAreCopied(t *testing.T) {
	targets := []string{"one"}
	token := encodeListCursor(targets, "one", "", nil, "/api/v1/pods?")
	targets[0] = "changed"

	index, _, _, err := decodeListCursor(token, []string{"one"}, "/api/v1/pods?")
	if err != nil || index != 0 {
		t.Fatalf("decoded copied targets = index %d, error %v", index, err)
	}
	if slices.Equal(targets, []string{"one"}) {
		t.Fatal("test did not mutate input targets")
	}
}
