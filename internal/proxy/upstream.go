package proxy

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

func (p *Proxy) requestAllTargets(ctx context.Context, original *http.Request, body []byte) []upstreamResponse {
	return p.requestTargets(ctx, p.targets, original, body)
}

func (p *Proxy) requestTargets(ctx context.Context, targets []Target, original *http.Request, body []byte) []upstreamResponse {
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
			responses[i] = p.requestTarget(ctx, target, original, targetBody)
		}()
	}
	wg.Wait()
	return responses
}

func (p *Proxy) requestTarget(ctx context.Context, target Target, original *http.Request, body []byte) upstreamResponse {
	upstreamURL := buildUpstreamURL(target.Host, original.URL)

	var lastResponse upstreamResponse
	for attempt := 0; attempt <= p.options.Retries; attempt++ {
		response := p.requestTargetOnce(ctx, target, original, upstreamURL, body)
		lastResponse = response
		if !shouldRetry(response) || attempt == p.options.Retries {
			return response
		}
		if !sleepWithContext(ctx, p.options.RetryBackoff) {
			return upstreamResponse{target: target, err: ctx.Err()}
		}
	}

	return lastResponse
}

func (p *Proxy) requestTargetOnce(ctx context.Context, target Target, original *http.Request, upstreamURL *url.URL, body []byte) upstreamResponse {
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

	request, err := newUpstreamRequest(requestCtx, target, original, upstreamURL, requestBody)
	if err != nil {
		return upstreamResponse{target: target, err: err}
	}

	response, err := target.Client.Do(request) // #nosec G704 -- proxying requests to selected kubeconfig targets is the purpose of this package.
	if err != nil {
		return upstreamResponse{target: target, err: err}
	}
	defer response.Body.Close()

	responseBody, err := readLimitedBody(response.Body, maxUpstreamResponseBodyBytes, "upstream response body")
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

func newUpstreamRequest(ctx context.Context, target Target, original *http.Request, upstreamURL *url.URL, body io.Reader) (*http.Request, error) {
	request, err := http.NewRequestWithContext(ctx, original.Method, upstreamURL.String(), body) // #nosec G704 -- upstream URL is built from a selected kubeconfig target by design.
	if err != nil {
		return nil, err
	}
	copyHeaders(request.Header, original.Header)
	request.Header.Del("Authorization")
	request.Header.Del("Accept-Encoding")
	request.Host = target.Host.Host
	return request, nil
}

func requestAcceptingJSONOnly(original *http.Request) *http.Request {
	request := original.Clone(original.Context())
	request.Header = original.Header.Clone()

	accepts := make([]string, 0, len(request.Header.Values("Accept")))
	for _, header := range request.Header.Values("Accept") {
		for _, mediaRange := range strings.Split(header, ",") {
			mediaRange = strings.TrimSpace(mediaRange)
			if mediaRange == "" || isKubernetesProtobufMediaRange(mediaRange) {
				continue
			}
			accepts = append(accepts, mediaRange)
		}
	}
	if len(accepts) == 0 {
		accepts = []string{"application/json"}
	}
	request.Header.Set("Accept", strings.Join(accepts, ", "))
	return request
}

func isKubernetesProtobufMediaRange(mediaRange string) bool {
	mediaType, _, _ := strings.Cut(mediaRange, ";")
	return strings.EqualFold(strings.TrimSpace(mediaType), "application/vnd.kubernetes.protobuf")
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

func singleJoiningSlash(firstPath, secondPath string) string {
	hasTrailingSlash := strings.HasSuffix(firstPath, "/")
	hasLeadingSlash := strings.HasPrefix(secondPath, "/")
	switch {
	case hasTrailingSlash && hasLeadingSlash:
		return firstPath + secondPath[1:]
	case !hasTrailingSlash && !hasLeadingSlash:
		return firstPath + "/" + secondPath
	default:
		return firstPath + secondPath
	}
}
