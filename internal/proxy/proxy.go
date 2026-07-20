package proxy

import (
	"bufio"
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"
)

const (
	DefaultRetries                   = 5
	contextNameAnnotation            = "kubeconfig-proxy.io/context-name"
	singleContextAnnotation          = "kubeconfig-proxy.io/single-context"
	sourceContextAnnotation          = "kubeconfig-proxy.io/context"
	sourceContextLabel               = "context"
	aggregateResourceVersionPrefix   = "kubeconfig-proxy:"
	aggregateResourceVersionQueryKey = "resourceVersion"
)

type Proxy struct {
	targets []Target
	primary Target
	options Options
}

type upstreamResponse struct {
	target Target
	status int
	header http.Header
	body   []byte
	err    error
}

type watchStream struct {
	target   Target
	header   http.Header
	response *http.Response
	cancel   context.CancelFunc
}

type watchOpenResult struct {
	stream   watchStream
	response upstreamResponse
	failed   bool
}

type Options struct {
	RequestTimeout   time.Duration
	Retries          int
	RetryBackoff     time.Duration
	BearerToken      string
	HelmReleaseProxy bool
	ReadOnly         bool
}

func New(targets []Target, primary Target) (*Proxy, error) {
	return NewWithOptions(targets, primary, Options{
		RequestTimeout: 30 * time.Second,
		Retries:        DefaultRetries,
		RetryBackoff:   200 * time.Millisecond,
	})
}

func NewWithOptions(targets []Target, primary Target, options Options) (*Proxy, error) {
	if len(targets) == 0 {
		return nil, fmt.Errorf("at least one target is required")
	}
	if options.Retries < 0 {
		return nil, fmt.Errorf("retries must be greater than or equal to 0")
	}
	if options.RequestTimeout < 0 {
		return nil, fmt.Errorf("request timeout must be greater than or equal to 0")
	}
	if options.RetryBackoff < 0 {
		return nil, fmt.Errorf("retry backoff must be greater than or equal to 0")
	}
	if options.BearerToken == "" {
		return nil, fmt.Errorf("bearer token is required")
	}
	return &Proxy{targets: targets, primary: primary, options: options}, nil
}

func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !p.authorized(r) {
		writeStatusError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if p.options.ReadOnly && isMutating(r.Method) {
		writeStatusError(w, http.StatusForbidden, "read-only proxy rejects mutating requests")
		return
	}

	switch {
	case isWatchRequest(r) && p.options.HelmReleaseProxy && isHelmStorageListRequest(r):
		p.streamSingle(w, r, p.primary)
	case isWatchRequest(r):
		p.aggregateWatch(w, r)
	case isLongRunningRequest(r):
		p.forwardLongRunning(w, r)
	case p.shouldUsePrimaryOnly(r):
		p.forwardSingle(w, r, p.primary)
	case isNamedResourceGetRequest(r):
		p.forwardExistingObject(w, r)
	case isAggregatableListRequest(r):
		p.aggregateList(w, r)
	case isMutating(r.Method):
		p.fanOut(w, r)
	default:
		p.forwardSingle(w, r, p.primary)
	}
}

func (p *Proxy) aggregateWatch(w http.ResponseWriter, r *http.Request) {
	empty, failed := p.selectedWatchIsEmpty(r)
	if failed != nil {
		if failed.err != nil {
			writeStatusError(w, http.StatusBadGateway, failed.err.Error())
			return
		}
		writeUpstreamResponse(w, *failed)
		return
	}
	if empty {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		return
	}

	streams, failed := p.openWatchStreams(r.Context(), r)
	if failed != nil {
		for _, stream := range streams {
			closeWatchStream(stream)
		}
		if failed.err != nil {
			writeStatusError(w, http.StatusBadGateway, failed.err.Error())
			return
		}
		writeUpstreamResponse(w, *failed)
		return
	}
	defer func() {
		for _, stream := range streams {
			closeWatchStream(stream)
		}
	}()

	if len(streams) == 0 {
		writeStatusError(w, http.StatusBadGateway, "no watch streams opened")
		return
	}

	copyHeaders(w.Header(), streams[0].header)
	if w.Header().Get("Content-Type") == "" {
		w.Header().Set("Content-Type", "application/json")
	}
	w.WriteHeader(http.StatusOK)

	flusher, _ := w.(http.Flusher)
	var writeMu sync.Mutex
	var wg sync.WaitGroup
	for _, stream := range streams {
		stream := stream
		wg.Add(1)
		go func() {
			defer wg.Done()
			copyWatchStream(r.Context(), stream, w, flusher, &writeMu)
		}()
	}
	wg.Wait()
}

func (p *Proxy) selectedWatchIsEmpty(r *http.Request) (bool, *upstreamResponse) {
	if !isNamedFieldSelector(r.URL.Query().Get("fieldSelector")) {
		return false, nil
	}

	listURL := *r.URL
	query := listURL.Query()
	for _, key := range []string{"watch", "resourceVersion", "resourceVersionMatch", "allowWatchBookmarks", "timeoutSeconds", "sendInitialEvents"} {
		query.Del(key)
	}
	listURL.RawQuery = query.Encode()

	listRequest := r.Clone(r.Context())
	listRequest.Method = http.MethodGet
	listRequest.URL = &listURL
	listRequest.Body = nil
	listRequest.ContentLength = 0

	responses := p.doAll(r.Context(), listRequest, nil)
	for _, response := range responses {
		if response.err != nil {
			return false, &response
		}
		if response.status < 200 || response.status >= 300 {
			return false, &response
		}

		var payload map[string]any
		if err := json.Unmarshal(response.body, &payload); err != nil {
			return false, &upstreamResponse{target: response.target, err: err}
		}
		items, ok := payload["items"].([]any)
		if !ok || len(items) > 0 {
			return false, nil
		}
	}
	return true, nil
}

func (p *Proxy) openWatchStreams(ctx context.Context, original *http.Request) ([]watchStream, *upstreamResponse) {
	results := make([]watchOpenResult, len(p.targets))
	var wg sync.WaitGroup
	for i, target := range p.targets {
		i, target := i, target
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i] = p.openWatchStream(ctx, original, target)
		}()
	}
	wg.Wait()

	streams := make([]watchStream, 0, len(results))
	for _, result := range results {
		if result.failed {
			return streams, &result.response
		}
		streams = append(streams, result.stream)
	}
	return streams, nil
}

func (p *Proxy) openWatchStream(ctx context.Context, original *http.Request, target Target) watchOpenResult {
	requestCtx, cancel := context.WithCancel(ctx)
	timer := (*time.Timer)(nil)
	if p.options.RequestTimeout > 0 {
		timer = time.AfterFunc(p.options.RequestTimeout, cancel)
	}

	upstreamURL := buildUpstreamURL(target.Host, original.URL)
	applyAggregateResourceVersion(upstreamURL, target.Name)
	request, err := http.NewRequestWithContext(requestCtx, original.Method, upstreamURL.String(), nil) // #nosec G704 -- upstream URL is built from a selected kubeconfig target by design.
	if err != nil {
		cancel()
		if timer != nil {
			timer.Stop()
		}
		return failedWatchOpen(target, err)
	}
	copyHeaders(request.Header, original.Header)
	request.Header.Del("Authorization")
	request.Header.Del("Accept-Encoding")
	request.Host = target.Host.Host

	response, err := target.Client.Do(request) // #nosec G704 -- proxying requests to selected kubeconfig targets is the purpose of this package.
	if err != nil {
		if requestCtx.Err() != nil && ctx.Err() == nil {
			err = context.DeadlineExceeded
		}
		cancel()
		if timer != nil {
			timer.Stop()
		}
		return failedWatchOpen(target, err)
	}
	if timer != nil && !timer.Stop() {
		_ = response.Body.Close()
		cancel()
		return failedWatchOpen(target, context.DeadlineExceeded)
	}

	if response.StatusCode < 200 || response.StatusCode >= 300 {
		body, readErr := io.ReadAll(response.Body)
		_ = response.Body.Close()
		cancel()
		if readErr != nil {
			return failedWatchOpen(target, readErr)
		}
		return watchOpenResult{
			response: upstreamResponse{
				target: target,
				status: response.StatusCode,
				header: response.Header.Clone(),
				body:   body,
			},
			failed: true,
		}
	}

	return watchOpenResult{
		stream: watchStream{
			target:   target,
			header:   response.Header.Clone(),
			response: response,
			cancel:   cancel,
		},
	}
}

func failedWatchOpen(target Target, err error) watchOpenResult {
	return watchOpenResult{
		response: upstreamResponse{target: target, err: err},
		failed:   true,
	}
}

func closeWatchStream(stream watchStream) {
	_ = stream.response.Body.Close()
	stream.cancel()
}

func copyWatchStream(ctx context.Context, stream watchStream, w io.Writer, flusher http.Flusher, writeMu *sync.Mutex) {
	reader := bufio.NewReader(stream.response.Body)
	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			line = markWatchEventSource(line, stream.target.Name)
			writeMu.Lock()
			_, _ = w.Write(line)
			if flusher != nil {
				flusher.Flush()
			}
			writeMu.Unlock()
		}
		if err != nil || ctx.Err() != nil {
			return
		}
	}
}

func (p *Proxy) authorized(r *http.Request) bool {
	const prefix = "Bearer "

	header := r.Header.Get("Authorization")
	if !strings.HasPrefix(header, prefix) {
		return false
	}

	got := strings.TrimPrefix(header, prefix)
	return subtle.ConstantTimeCompare([]byte(got), []byte(p.options.BearerToken)) == 1
}

func (p *Proxy) forwardLongRunning(w http.ResponseWriter, r *http.Request) {
	target := p.primary
	if objectPath, ok := podObjectPathForSubresource(r.URL.Path); ok {
		if found, foundOK := p.targetForExistingObject(r.Context(), r, objectPath); foundOK {
			target = found
		}
	}
	p.streamSingle(w, r, target)
}

func (p *Proxy) forwardExistingObject(w http.ResponseWriter, r *http.Request) {
	target := p.primary
	if found, ok := p.targetForExistingObject(r.Context(), r, r.URL.Path); ok {
		target = found
	}
	p.forwardSingle(w, r, target)
}

func (p *Proxy) forwardSingle(w http.ResponseWriter, r *http.Request, target Target) {
	if isLongRunningRequest(r) {
		p.streamSingle(w, r, target)
		return
	}

	response := p.do(r.Context(), target, r, nil)
	if response.err != nil {
		writeStatusError(w, http.StatusBadGateway, response.err.Error())
		return
	}
	writeUpstreamResponse(w, response)
}

func (p *Proxy) targetForExistingObject(ctx context.Context, original *http.Request, objectPath string) (Target, bool) {
	for _, target := range p.targets {
		objectURL := *original.URL
		objectURL.Path = objectPath
		objectURL.RawQuery = ""

		request := original.Clone(ctx)
		request.Method = http.MethodGet
		request.URL = &objectURL
		request.Body = nil
		request.ContentLength = 0

		response := p.do(ctx, target, request, nil)
		if response.err == nil && response.status >= 200 && response.status < 300 {
			return target, true
		}
	}
	return Target{}, false
}

func (p *Proxy) streamSingle(w http.ResponseWriter, r *http.Request, target Target) {
	reverseProxy := &httputil.ReverseProxy{
		Transport:     target.Client.Transport,
		FlushInterval: -1,
		Rewrite: func(proxyRequest *httputil.ProxyRequest) {
			upstreamURL := buildUpstreamURL(target.Host, proxyRequest.In.URL)
			proxyRequest.Out.URL = upstreamURL
			proxyRequest.Out.Host = target.Host.Host
			proxyRequest.Out.Header.Del("Authorization")
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			writeStatusError(w, http.StatusBadGateway, err.Error())
		},
	}
	reverseProxy.ServeHTTP(w, r)
}

func (p *Proxy) aggregateList(w http.ResponseWriter, r *http.Request) {
	responses := p.doAll(r.Context(), r, nil)
	okResponses := make([]upstreamResponse, 0, len(responses))
	for _, response := range responses {
		if response.err != nil {
			writeStatusError(w, http.StatusBadGateway, fmt.Sprintf("%s: %v", response.target.Name, response.err))
			return
		}
		if response.status < 200 || response.status >= 300 {
			writeUpstreamResponse(w, response)
			return
		}
		okResponses = append(okResponses, response)
	}

	merged, err := mergeLists(okResponses)
	if err != nil {
		writeStatusError(w, http.StatusBadGateway, err.Error())
		return
	}
	copyHeaders(w.Header(), okResponses[0].header)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(merged) // #nosec G705 -- response body is Kubernetes API JSON, not browser-rendered HTML.
}

func (p *Proxy) fanOut(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeStatusError(w, http.StatusBadRequest, err.Error())
		return
	}

	targets, err := p.targetsForMutationRequest(r.Context(), r, body)
	if err != nil {
		writeStatusError(w, http.StatusBadRequest, err.Error())
		return
	}

	responses := p.doAllToTargets(r.Context(), targets, r, body)
	for _, response := range responses {
		if response.err != nil {
			writeStatusError(w, http.StatusBadGateway, fmt.Sprintf("%s: %v", response.target.Name, response.err))
			return
		}
		if response.status < 200 || response.status >= 300 {
			writeUpstreamResponse(w, response)
			return
		}
	}
	writeUpstreamResponse(w, responses[0])
}

func (p *Proxy) doAll(ctx context.Context, original *http.Request, body []byte) []upstreamResponse {
	return p.doAllToTargets(ctx, p.targets, original, body)
}

func (p *Proxy) doAllToTargets(ctx context.Context, targets []Target, original *http.Request, body []byte) []upstreamResponse {
	responses := make([]upstreamResponse, len(targets))
	var wg sync.WaitGroup
	for i, target := range targets {
		i, target := i, target
		wg.Add(1)
		go func() {
			defer wg.Done()
			targetBody, err := p.bodyForTarget(ctx, target, original, body)
			if err != nil {
				responses[i] = upstreamResponse{target: target, err: err}
				return
			}
			responses[i] = p.do(ctx, target, original, targetBody)
		}()
	}
	wg.Wait()
	return responses
}

func (p *Proxy) bodyForTarget(ctx context.Context, target Target, original *http.Request, body []byte) ([]byte, error) {
	if original.Method != http.MethodPut || len(body) == 0 {
		return body, nil
	}

	getRequest := original.Clone(ctx)
	getRequest.Method = http.MethodGet
	getRequest.Body = nil
	getRequest.ContentLength = 0
	response := p.do(ctx, target, getRequest, nil)
	if response.err != nil {
		return nil, response.err
	}
	if response.status < 200 || response.status >= 300 {
		return body, nil
	}

	rewritten, err := rewriteObjectIdentity(body, response.body)
	if err != nil {
		return nil, err
	}
	return rewritten, nil
}

func (p *Proxy) do(ctx context.Context, target Target, original *http.Request, body []byte) upstreamResponse {
	upstreamURL := buildUpstreamURL(target.Host, original.URL)

	var last upstreamResponse
	for attempt := 0; attempt <= p.options.Retries; attempt++ {
		response := p.doOnce(ctx, target, original, upstreamURL, body)
		last = response
		if !shouldRetry(response) || attempt == p.options.Retries {
			return response
		}
		if !sleepWithContext(ctx, p.options.RetryBackoff) {
			return upstreamResponse{target: target, err: ctx.Err()}
		}
	}

	return last
}

func (p *Proxy) doOnce(ctx context.Context, target Target, original *http.Request, upstreamURL *url.URL, body []byte) upstreamResponse {
	requestCtx := ctx
	cancel := func() {}
	if p.options.RequestTimeout > 0 && shouldUseRequestTimeout(original) {
		requestCtx, cancel = context.WithTimeout(ctx, p.options.RequestTimeout)
	}
	defer cancel()

	requestBody := io.Reader(nil)
	if body != nil {
		requestBody = bytes.NewReader(body)
	}

	request, err := http.NewRequestWithContext(requestCtx, original.Method, upstreamURL.String(), requestBody) // #nosec G704 -- upstream URL is built from a selected kubeconfig target by design.
	if err != nil {
		return upstreamResponse{target: target, err: err}
	}
	copyHeaders(request.Header, original.Header)
	request.Header.Del("Authorization")
	request.Header.Del("Accept-Encoding")
	request.Host = target.Host.Host

	response, err := target.Client.Do(request) // #nosec G704 -- proxying requests to selected kubeconfig targets is the purpose of this package.
	if err != nil {
		return upstreamResponse{target: target, err: err}
	}
	defer response.Body.Close()

	responseBody, err := io.ReadAll(response.Body)
	if err != nil {
		return upstreamResponse{target: target, err: err}
	}

	return upstreamResponse{
		target: target,
		status: response.StatusCode,
		header: response.Header.Clone(),
		body:   responseBody,
	}
}

func shouldRetry(response upstreamResponse) bool {
	if response.err != nil {
		return true
	}
	switch response.status {
	case http.StatusTooManyRequests,
		http.StatusInternalServerError,
		http.StatusBadGateway,
		http.StatusServiceUnavailable,
		http.StatusGatewayTimeout:
		return true
	default:
		return false
	}
}

func sleepWithContext(ctx context.Context, delay time.Duration) bool {
	if delay <= 0 {
		return true
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func buildUpstreamURL(host *url.URL, requestURL *url.URL) *url.URL {
	upstreamURL := *host
	upstreamURL.Path = singleJoiningSlash(host.Path, requestURL.Path)
	upstreamURL.RawQuery = requestURL.RawQuery
	return &upstreamURL
}

func mergeLists(responses []upstreamResponse) ([]byte, error) {
	var merged map[string]any
	resourceVersions := map[string]string{}
	for _, response := range responses {
		var payload map[string]any
		if err := json.Unmarshal(response.body, &payload); err != nil {
			return nil, fmt.Errorf("%s: decode list response: %w", response.target.Name, err)
		}
		if resourceVersion := payloadResourceVersion(payload); resourceVersion != "" {
			resourceVersions[response.target.Name] = resourceVersion
		}

		switch {
		case hasArray(payload, "items"):
			mergeListItems(payload, &merged, response.target.Name)
		case hasArray(payload, "rows"):
			mergeTableRows(payload, &merged, response.target.Name)
		default:
			return response.body, nil
		}
	}

	if merged != nil {
		metadata := ensureMap(merged, "metadata")
		metadata["resourceVersion"] = encodeAggregateResourceVersion(resourceVersions)
	}
	return json.Marshal(merged)
}

func mergeListItems(payload map[string]any, merged *map[string]any, contextName string) {
	items, _ := payload["items"].([]any)
	for i := range items {
		item, ok := items[i].(map[string]any)
		if !ok {
			continue
		}
		metadata := ensureMap(item, "metadata")
		markSourceContext(metadata, contextName)
	}

	if *merged == nil {
		*merged = payload
		(*merged)["items"] = items
		return
	}

	mergedItems, _ := (*merged)["items"].([]any)
	(*merged)["items"] = append(mergedItems, items...)
}

func mergeTableRows(payload map[string]any, merged *map[string]any, contextName string) {
	rows, _ := payload["rows"].([]any)
	for i := range rows {
		row, ok := rows[i].(map[string]any)
		if !ok {
			continue
		}
		if object, ok := row["object"].(map[string]any); ok {
			metadata := ensureMap(object, "metadata")
			markSourceContext(metadata, contextName)
		}
	}

	if *merged == nil {
		*merged = payload
		(*merged)["rows"] = rows
		return
	}

	mergedRows, _ := (*merged)["rows"].([]any)
	(*merged)["rows"] = append(mergedRows, rows...)
}

func markSourceContext(metadata map[string]any, contextName string) {
	annotations := ensureMap(metadata, "annotations")
	annotations[sourceContextAnnotation] = contextName

	labels := ensureMap(metadata, "labels")
	labels[sourceContextLabel] = contextName
}

func markWatchEventSource(line []byte, contextName string) []byte {
	trimmed := bytes.TrimSpace(line)
	if len(trimmed) == 0 {
		return line
	}

	var event map[string]any
	if err := json.Unmarshal(trimmed, &event); err != nil {
		return line
	}
	object, ok := event["object"].(map[string]any)
	if !ok {
		return line
	}
	metadata, ok := object["metadata"].(map[string]any)
	if !ok {
		return line
	}
	markSourceContext(metadata, contextName)

	encoded, err := json.Marshal(event)
	if err != nil {
		return line
	}
	return append(encoded, '\n')
}

func ensureMap(parent map[string]any, key string) map[string]any {
	child, ok := parent[key].(map[string]any)
	if !ok {
		child = map[string]any{}
		parent[key] = child
	}
	return child
}

func hasArray(payload map[string]any, key string) bool {
	_, ok := payload[key].([]any)
	return ok
}

func payloadResourceVersion(payload map[string]any) string {
	metadata, ok := payload["metadata"].(map[string]any)
	if !ok {
		return ""
	}
	resourceVersion, _ := metadata["resourceVersion"].(string)
	return resourceVersion
}

func encodeAggregateResourceVersion(resourceVersions map[string]string) string {
	if len(resourceVersions) == 0 {
		return ""
	}
	payload, err := json.Marshal(resourceVersions)
	if err != nil {
		return ""
	}
	return aggregateResourceVersionPrefix + base64.RawURLEncoding.EncodeToString(payload)
}

func decodeAggregateResourceVersion(resourceVersion string) (map[string]string, bool) {
	if !strings.HasPrefix(resourceVersion, aggregateResourceVersionPrefix) {
		return nil, false
	}
	encoded := strings.TrimPrefix(resourceVersion, aggregateResourceVersionPrefix)
	payload, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return nil, false
	}
	var resourceVersions map[string]string
	if err := json.Unmarshal(payload, &resourceVersions); err != nil {
		return nil, false
	}
	return resourceVersions, true
}

func applyAggregateResourceVersion(upstreamURL *url.URL, targetName string) {
	query := upstreamURL.Query()
	resourceVersions, ok := decodeAggregateResourceVersion(query.Get(aggregateResourceVersionQueryKey))
	if !ok {
		return
	}
	if resourceVersion := resourceVersions[targetName]; resourceVersion != "" {
		query.Set(aggregateResourceVersionQueryKey, resourceVersion)
	} else {
		query.Del(aggregateResourceVersionQueryKey)
	}
	upstreamURL.RawQuery = query.Encode()
}

func (p *Proxy) targetsForMutationRequest(ctx context.Context, original *http.Request, body []byte) ([]Target, error) {
	if (original.Method == http.MethodDelete || original.Method == http.MethodPatch) && isNamedResourcePath(original.URL.Path) {
		if targets, ok, err := p.targetsForExistingResourceMutation(ctx, original); err != nil || ok {
			return targets, err
		}
	}
	return p.targetsForMutation(body)
}

func (p *Proxy) targetsForExistingResourceMutation(ctx context.Context, original *http.Request) ([]Target, bool, error) {
	foundTargets := make([]Target, 0, len(p.targets))
	for _, target := range p.targets {
		objectURL := *original.URL
		objectURL.RawQuery = ""

		request := original.Clone(ctx)
		request.Method = http.MethodGet
		request.URL = &objectURL
		request.Body = nil
		request.ContentLength = 0

		response := p.do(ctx, target, request, nil)
		if response.err != nil {
			return nil, false, response.err
		}
		if response.status == http.StatusNotFound {
			continue
		}
		if response.status < 200 || response.status >= 300 {
			return nil, false, fmt.Errorf("%s: get existing resource before mutation returned HTTP %d", target.Name, response.status)
		}

		targets, err := p.targetsForMutation(response.body)
		if err != nil {
			return nil, false, err
		}
		if len(targets) != len(p.targets) {
			return targets, true, nil
		}
		foundTargets = append(foundTargets, target)
	}
	if len(foundTargets) > 0 && len(foundTargets) != len(p.targets) {
		return foundTargets, true, nil
	}
	return nil, false, nil
}

func (p *Proxy) targetsForMutation(body []byte) ([]Target, error) {
	annotations := resourceAnnotations(body)
	if contextName := annotations[contextNameAnnotation]; contextName != "" {
		target, ok := p.targetByName(contextName)
		if !ok {
			return nil, fmt.Errorf("context %q from annotation %q is not configured in proxy", contextName, contextNameAnnotation)
		}
		return []Target{target}, nil
	}
	if strings.EqualFold(strings.TrimSpace(annotations[singleContextAnnotation]), "true") {
		return []Target{p.firstTargetByName()}, nil
	}
	return p.targets, nil
}

func (p *Proxy) targetByName(name string) (Target, bool) {
	for _, target := range p.targets {
		if target.Name == name {
			return target, true
		}
	}
	return Target{}, false
}

func (p *Proxy) firstTargetByName() Target {
	first := p.targets[0]
	for _, target := range p.targets[1:] {
		if target.Name < first.Name {
			first = target
		}
	}
	return first
}

func resourceAnnotations(body []byte) map[string]string {
	if len(body) == 0 {
		return nil
	}

	var resource map[string]any
	if err := json.Unmarshal(body, &resource); err != nil {
		jsonBody, yamlErr := yaml.YAMLToJSON(body)
		if yamlErr != nil {
			return nil
		}
		if err := json.Unmarshal(jsonBody, &resource); err != nil {
			return nil
		}
	}
	metadata, ok := resource["metadata"].(map[string]any)
	if !ok {
		return nil
	}
	rawAnnotations, ok := metadata["annotations"].(map[string]any)
	if !ok {
		return nil
	}

	annotations := make(map[string]string, len(rawAnnotations))
	for key, value := range rawAnnotations {
		if stringValue, ok := value.(string); ok {
			annotations[key] = stringValue
		}
	}
	return annotations
}

func rewriteObjectIdentity(body, currentBody []byte) ([]byte, error) {
	desired, err := decodeObject(body)
	if err != nil {
		return body, nil
	}
	current, err := decodeObject(currentBody)
	if err != nil {
		return body, nil
	}

	desiredMetadata := ensureMap(desired, "metadata")
	currentMetadata, ok := current["metadata"].(map[string]any)
	if !ok {
		return body, nil
	}

	for _, key := range []string{"uid", "resourceVersion"} {
		if value, ok := currentMetadata[key]; ok {
			desiredMetadata[key] = value
		}
	}

	rewritten, err := json.Marshal(desired)
	if err != nil {
		return nil, err
	}
	return rewritten, nil
}

func decodeObject(body []byte) (map[string]any, error) {
	var object map[string]any
	if err := json.Unmarshal(body, &object); err == nil {
		return object, nil
	}

	jsonBody, err := yaml.YAMLToJSON(body)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(jsonBody, &object); err != nil {
		return nil, err
	}
	return object, nil
}

func (p *Proxy) shouldUsePrimaryOnly(r *http.Request) bool {
	return isDiscoveryPath(r.URL.Path) || p.options.HelmReleaseProxy && isHelmStorageListRequest(r)
}

func shouldUseRequestTimeout(r *http.Request) bool {
	return !isLongRunningRequest(r)
}

func isLongRunningRequest(r *http.Request) bool {
	if isWatchRequest(r) {
		return true
	}
	_, ok := podObjectPathForSubresource(r.URL.Path)
	return ok
}

func podObjectPathForSubresource(path string) (string, bool) {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) != 7 {
		return "", false
	}
	if parts[0] != "api" || parts[2] != "namespaces" || parts[4] != "pods" {
		return "", false
	}
	switch parts[6] {
	case "attach", "exec", "log", "portforward":
		return "/" + strings.Join(parts[:6], "/"), true
	default:
		return "", false
	}
}

func isAggregatableListRequest(r *http.Request) bool {
	return r.Method == http.MethodGet && !isDiscoveryPath(r.URL.Path) && r.URL.Query().Get("watch") != "true"
}

func isWatchRequest(r *http.Request) bool {
	return r.Method == http.MethodGet && r.URL.Query().Get("watch") == "true"
}

func isHelmStorageListRequest(r *http.Request) bool {
	if r.Method != http.MethodGet {
		return false
	}
	if !isCoreResourceListPath(r.URL.Path, "secrets", "configmaps") {
		return false
	}
	return labelSelectorHas(r.URL.Query().Get("labelSelector"), "owner", "helm")
}

func isCoreResourceListPath(path string, resources ...string) bool {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	resource := ""
	switch {
	case len(parts) == 3 && parts[0] == "api" && parts[1] == "v1":
		resource = parts[2]
	case len(parts) == 5 && parts[0] == "api" && parts[1] == "v1" && parts[2] == "namespaces":
		resource = parts[4]
	default:
		return false
	}

	for _, candidate := range resources {
		if resource == candidate {
			return true
		}
	}
	return false
}

func labelSelectorHas(selector, key, value string) bool {
	for _, requirement := range strings.Split(selector, ",") {
		requirement = strings.TrimSpace(requirement)
		if requirement == key+"="+value || requirement == key+"=="+value {
			return true
		}
	}
	return false
}

func isNamedFieldSelector(selector string) bool {
	for _, requirement := range strings.Split(selector, ",") {
		requirement = strings.TrimSpace(requirement)
		if strings.HasPrefix(requirement, "metadata.name=") || strings.HasPrefix(requirement, "metadata.name==") {
			return true
		}
	}
	return false
}

func isNamedResourceGetRequest(r *http.Request) bool {
	return r.Method == http.MethodGet && isNamedResourcePath(r.URL.Path)
}

func isNamedResourcePath(path string) bool {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) == 4 && parts[0] == "api" {
		return true
	}
	if len(parts) == 6 && parts[0] == "api" && parts[2] == "namespaces" {
		return true
	}
	if len(parts) == 5 && parts[0] == "apis" {
		return true
	}
	return len(parts) == 7 && parts[0] == "apis" && parts[3] == "namespaces"
}

func isMutating(method string) bool {
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	default:
		return false
	}
}

func isDiscoveryPath(path string) bool {
	return path == "/api" ||
		path == "/apis" ||
		path == "/version" ||
		strings.HasPrefix(path, "/api/") && strings.Count(strings.Trim(path, "/"), "/") <= 1 ||
		strings.HasPrefix(path, "/apis/") && strings.Count(strings.Trim(path, "/"), "/") <= 2 ||
		strings.HasPrefix(path, "/openapi") ||
		strings.HasPrefix(path, "/swagger") ||
		strings.HasPrefix(path, "/healthz") ||
		strings.HasPrefix(path, "/livez") ||
		strings.HasPrefix(path, "/readyz")
}

func writeUpstreamResponse(w http.ResponseWriter, response upstreamResponse) {
	copyHeaders(w.Header(), response.header)
	w.WriteHeader(response.status)
	_, _ = w.Write(response.body)
}

func writeStatusError(w http.ResponseWriter, code int, message string) {
	statusCode := int32(http.StatusInternalServerError)
	if code >= 100 && code <= 599 {
		statusCode = int32(code)
	}
	status := metav1.Status{
		TypeMeta: metav1.TypeMeta{Kind: "Status", APIVersion: "v1"},
		Status:   metav1.StatusFailure,
		Code:     statusCode,
		Message:  message,
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(status)
}

func copyHeaders(dst, src http.Header) {
	for key, values := range src {
		if isHopByHopHeader(key) {
			continue
		}
		dst.Del(key)
		for _, value := range values {
			dst.Add(key, value)
		}
	}
}

func isHopByHopHeader(key string) bool {
	switch strings.ToLower(key) {
	case "connection", "content-length", "keep-alive", "proxy-authenticate", "proxy-authorization", "te", "trailer", "transfer-encoding", "upgrade":
		return true
	default:
		return false
	}
}

func singleJoiningSlash(a, b string) string {
	aslash := strings.HasSuffix(a, "/")
	bslash := strings.HasPrefix(b, "/")
	switch {
	case aslash && bslash:
		return a + b[1:]
	case !aslash && !bslash:
		return a + "/" + b
	default:
		return a + b
	}
}
