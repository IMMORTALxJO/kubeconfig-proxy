package proxy

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	neturl "net/url"
	"strings"
	"sync"

	"k8s.io/apimachinery/pkg/fields"
)

type watchStream struct {
	target   Target
	response *http.Response
	cancel   context.CancelFunc
}

func (p *Proxy) aggregateWatch(w http.ResponseWriter, r *http.Request) {
	resource := parseResourcePath(r.URL.Path)
	if namedWatch, ok := namedWatchRequest(r, resource); ok {
		p.aggregateNamedWatch(w, namedWatch)
		return
	}
	aggregateWatchTargets(w, r, p.targets)
}

func namedWatchRequest(original *http.Request, resource resourcePath) (*http.Request, bool) {
	if resource.isObject && resource.subresource == "" {
		return original, true
	}
	if !resource.isCollection {
		return nil, false
	}
	if _, ok := namedFieldSelector(original.URL.Query().Get("fieldSelector")); !ok {
		return nil, false
	}
	return original, true
}

func namedFieldSelector(value string) (string, bool) {
	selector, err := fields.ParseSelector(value)
	if err != nil {
		return "", false
	}
	name, ok := selector.RequiresExactMatch("metadata.name")
	return name, ok && name != ""
}

func (p *Proxy) aggregateNamedWatch(w http.ResponseWriter, r *http.Request) {
	resource := parseResourcePath(r.URL.Path)
	responses := p.probe(r.Context(), r, namedWatchProbePath(r, resource))
	targets, err := foundTargets(responses)
	if err != nil {
		writeStatus(w, http.StatusBadGateway, err.Error())
		return
	}
	if len(targets) == 0 {
		writeTargetFailure(w, firstFailure(responses))
		return
	}
	versions := make(map[string]string, len(targets))
	for _, response := range responses {
		if response.status < 200 || response.status >= 300 {
			continue
		}
		version, err := listResourceVersion(response.body)
		if err != nil {
			writeStatus(w, http.StatusBadGateway, response.target.Name+": "+err.Error())
			return
		}
		versions[response.target.Name] = version
	}
	aggregateWatchTargets(w, withAggregateResourceVersions(r, versions), targets)
}

func namedWatchProbePath(r *http.Request, resource resourcePath) string {
	if resource.isObject {
		return resource.ownerPath
	}
	name, _ := namedFieldSelector(r.URL.Query().Get("fieldSelector"))
	return strings.TrimSuffix(r.URL.Path, "/") + "/" + neturl.PathEscape(name)
}

func aggregateWatchTargets(w http.ResponseWriter, r *http.Request, targets []Target) {
	streams, failure := openWatches(r.Context(), r, targets)
	if failure.err != nil || failure.status < 200 || failure.status >= 300 {
		for _, stream := range streams {
			closeWatchStream(stream)
		}
		writeTargetFailure(w, failure)
		return
	}
	defer func() {
		for _, stream := range streams {
			closeWatchStream(stream)
		}
	}()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	var writeMu sync.Mutex
	var wg sync.WaitGroup
	for _, stream := range streams {
		stream := stream
		wg.Add(1)
		go func() { defer wg.Done(); copyWatch(r.Context(), w, &writeMu, stream) }()
	}
	wg.Wait()
}

func withAggregateResourceVersions(original *http.Request, versions map[string]string) *http.Request {
	request := original.Clone(original.Context())
	url := *original.URL
	query := url.Query()
	query.Set("resourceVersion", encodeAggregateResourceVersions(versions))
	url.RawQuery = query.Encode()
	request.URL = &url
	return request
}

func closeWatchStream(stream watchStream) {
	if stream.response != nil {
		_ = stream.response.Body.Close()
	}
	if stream.cancel != nil {
		stream.cancel()
	}
}

func openWatches(ctx context.Context, original *http.Request, targets []Target) ([]watchStream, upstreamResponse) {
	resourceVersions := aggregateResourceVersions(original.URL.Query().Get("resourceVersion"))
	results := make([]watchStream, len(targets))
	failures := make([]upstreamResponse, len(targets))
	var wg sync.WaitGroup
	for i, target := range targets {
		i, target := i, target
		wg.Add(1)
		go func() {
			defer wg.Done()
			requestCtx, cancel := context.WithCancel(ctx)
			requestOriginal := original
			if resourceVersions != nil {
				requestOriginal = original.Clone(requestCtx)
				url := *original.URL
				query := url.Query()
				query.Set("resourceVersion", resourceVersions[target.Name])
				url.RawQuery = query.Encode()
				requestOriginal.URL = &url
			}
			request, err := newRequest(requestCtx, target, requestOriginal, buildURL(target.Host, requestOriginal.URL), nil)
			if err != nil {
				cancel()
				failures[i] = upstreamResponse{target: target, err: err}
				return
			}
			response, err := target.Client.Do(request) // #nosec G704 -- target is selected from the configured kubeconfig contexts.
			if err != nil {
				cancel()
				failures[i] = upstreamResponse{target: target, err: err}
				return
			}
			if response.StatusCode < 200 || response.StatusCode >= 300 {
				_ = response.Body.Close()
				cancel()
				failures[i] = upstreamResponse{target: target, status: response.StatusCode, header: response.Header.Clone()}
				return
			}
			results[i] = watchStream{target: target, response: response, cancel: cancel}
		}()
	}
	wg.Wait()
	for _, failure := range failures {
		if failure.err != nil || failure.status != 0 {
			return results, failure
		}
	}
	return results, upstreamResponse{status: http.StatusOK}
}

func copyWatch(ctx context.Context, w http.ResponseWriter, mu *sync.Mutex, stream watchStream) {
	reader := bufio.NewReader(stream.response.Body)
	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			line = markEvent(line, stream.target.Name)
			mu.Lock()
			_, _ = w.Write(line)
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
			mu.Unlock()
		}
		if err != nil || ctx.Err() != nil {
			return
		}
	}
}

func markEvent(line []byte, contextName string) []byte {
	var event map[string]any
	if json.Unmarshal(line, &event) != nil {
		return line
	}
	markEntry(event["object"], contextName)
	result, err := json.Marshal(event)
	if err != nil {
		return line
	}
	return append(result, '\n')
}
