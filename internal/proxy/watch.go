package proxy

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
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
	p.aggregateWatchTargets(w, r, p.targets)
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
	initialEvents := make([][]byte, 0, len(targets))
	shouldSendInitialEvents := r.URL.Query().Get("resourceVersion") == ""
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
	if shouldSendInitialEvents {
		for _, response := range responses {
			if response.status >= 200 && response.status < 300 {
				initialEvents = append(initialEvents, initialWatchEvent(response.body, response.target.Name, versions))
			}
		}
	}
	p.aggregateWatchTargetsWithInitialEvents(w, withAggregateResourceVersions(r, versions), targets, initialEvents)
}

func namedWatchProbePath(r *http.Request, resource resourcePath) string {
	if resource.isObject {
		return resource.ownerPath
	}
	name, _ := namedFieldSelector(r.URL.Query().Get("fieldSelector"))
	return strings.TrimSuffix(r.URL.Path, "/") + "/" + neturl.PathEscape(name)
}

func (p *Proxy) aggregateWatchTargets(w http.ResponseWriter, r *http.Request, targets []Target) {
	p.aggregateWatchTargetsWithInitialEvents(w, r, targets, nil)
}

func (p *Proxy) aggregateWatchTargetsWithInitialEvents(w http.ResponseWriter, r *http.Request, targets []Target, initialEvents [][]byte) {
	resourceVersions, failure, err := p.watchResourceVersions(r, targets)
	if err != nil {
		writeStatus(w, http.StatusBadRequest, err.Error())
		return
	}
	if failure.err != nil || failure.status != 0 {
		writeTargetFailure(w, failure)
		return
	}
	watchRequest := withAggregateResourceVersions(r, resourceVersions)
	watchCtx, cancelWatch := context.WithCancel(r.Context())
	defer cancelWatch()
	streams, failure := openWatches(watchCtx, watchRequest, targets)
	if failure.err != nil || failure.status < 200 || failure.status >= 300 {
		for _, stream := range streams {
			closeWatchStream(stream)
		}
		writeTargetFailure(w, failure)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	for _, event := range initialEvents {
		_, _ = w.Write(event)
	}
	if len(initialEvents) > 0 {
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
	}
	var writeMu sync.Mutex
	var wg sync.WaitGroup
	streamEnded := make(chan struct{}, len(streams))
	for _, stream := range streams {
		stream := stream
		wg.Add(1)
		go func() {
			defer wg.Done()
			copyWatch(watchCtx, w, &writeMu, stream, resourceVersions)
			streamEnded <- struct{}{}
		}()
	}
	<-streamEnded
	cancelWatch()
	for _, stream := range streams {
		closeWatchStream(stream)
	}
	wg.Wait()
}

func (p *Proxy) watchResourceVersions(r *http.Request, targets []Target) (map[string]string, upstreamResponse, error) {
	requestedVersion := r.URL.Query().Get("resourceVersion")
	if strings.HasPrefix(requestedVersion, aggregateResourceVersionPrefix) {
		versions := aggregateResourceVersions(requestedVersion)
		if versions == nil {
			return nil, upstreamResponse{}, fmt.Errorf("invalid aggregate watch resource version")
		}
		validated, err := resourceVersionsForTargets(versions, targets)
		if err != nil {
			return nil, upstreamResponse{}, err
		}
		return validated, upstreamResponse{}, nil
	}
	if requestedVersion != "" {
		versions := make(map[string]string, len(targets))
		for _, target := range targets {
			versions[target.Name] = requestedVersion
		}
		return versions, upstreamResponse{}, nil
	}

	responses := p.requestTargets(r.Context(), targets, watchVersionRequest(r), nil)
	versions := make(map[string]string, len(responses))
	for _, response := range responses {
		if response.err != nil || response.status < 200 || response.status >= 300 {
			return nil, response, nil
		}
		version, err := listResourceVersion(response.body)
		if err != nil {
			return nil, upstreamResponse{target: response.target, err: err}, nil
		}
		versions[response.target.Name] = version
	}
	return versions, upstreamResponse{}, nil
}

func resourceVersionsForTargets(versions map[string]string, targets []Target) (map[string]string, error) {
	validated := make(map[string]string, len(targets))
	for _, target := range targets {
		version := versions[target.Name]
		if version == "" {
			return nil, fmt.Errorf("missing watch resource version for context %q", target.Name)
		}
		validated[target.Name] = version
	}
	return validated, nil
}

func watchVersionRequest(r *http.Request) *http.Request {
	request := r.Clone(r.Context())
	url := *r.URL
	query := url.Query()
	query.Del("watch")
	query.Del("timeoutSeconds")
	query.Del("allowWatchBookmarks")
	query.Del("sendInitialEvents")
	query.Del("continue")
	query.Del("resourceVersionMatch")
	query.Set("limit", "1")
	query.Set("resourceVersion", "0")
	url.RawQuery = query.Encode()
	request.URL = &url
	return request
}

func initialWatchEvent(body []byte, contextName string, resourceVersions map[string]string) []byte {
	event := make([]byte, 0, len(body)+24)
	event = append(event, `{"type":"ADDED","object":`...)
	event = append(event, body...)
	event = append(event, '}')
	return markEvent(event, contextName, resourceVersions)
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
	cancels := make([]context.CancelFunc, len(targets))
	completed := make(chan int, len(targets))
	var wg sync.WaitGroup
	for i, target := range targets {
		i, target := i, target
		requestCtx, cancel := context.WithCancel(ctx)
		cancels[i] = cancel
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { completed <- i }()
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
	failureIndex := -1
	for range targets {
		i := <-completed
		if failureIndex < 0 && (failures[i].err != nil || failures[i].status != 0) {
			failureIndex = i
			for _, cancel := range cancels {
				cancel()
			}
		}
	}
	wg.Wait()
	if failureIndex >= 0 {
		return results, failures[failureIndex]
	}
	return results, upstreamResponse{status: http.StatusOK}
}

func copyWatch(ctx context.Context, w http.ResponseWriter, mu *sync.Mutex, stream watchStream, resourceVersions map[string]string) {
	reader := bufio.NewReader(stream.response.Body)
	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			mu.Lock()
			line = markEvent(line, stream.target.Name, resourceVersions)
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

func markEvent(line []byte, contextName string, resourceVersions map[string]string) []byte {
	var event map[string]any
	if json.Unmarshal(line, &event) != nil {
		return line
	}
	if metadata, ok := entryMetadata(event["object"]); ok {
		if version, ok := metadata["resourceVersion"].(string); ok && version != "" && resourceVersions != nil {
			resourceVersions[contextName] = version
			metadata["resourceVersion"] = encodeAggregateResourceVersions(resourceVersions)
		}
	}
	markEntry(event["object"], contextName)
	result, err := json.Marshal(event)
	if err != nil {
		return line
	}
	return append(result, '\n')
}
