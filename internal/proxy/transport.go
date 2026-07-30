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

const maxBodyBytes = 64 << 20

func (p *Proxy) requestTarget(ctx context.Context, target Target, original *http.Request, body []byte) upstreamResponse {
	url := buildURL(target.Host, original.URL)
	var last upstreamResponse
	for attempt := 0; attempt <= p.options.Retries; attempt++ {
		requestCtx, cancel := ctx, func() {}
		if p.options.RequestTimeout > 0 && !isWatch(original) && !isPodConnection(parseResourcePath(original.URL.Path).subresource) {
			requestCtx, cancel = context.WithTimeout(ctx, p.options.RequestTimeout)
		}
		request, err := newRequest(requestCtx, target, original, url, body)
		if err != nil {
			cancel()
			return upstreamResponse{target: target, err: err}
		}
		response, err := target.Client.Do(request) // #nosec G704 -- targets originate in a selected kubeconfig.
		if err != nil {
			cancel()
			last = upstreamResponse{target: target, err: err}
		} else {
			data, readErr := readBody(response.Body, maxBodyBytes)
			_ = response.Body.Close()
			cancel()
			if readErr != nil {
				last = upstreamResponse{target: target, err: readErr}
			} else {
				last = upstreamResponse{target: target, status: response.StatusCode, header: response.Header.Clone(), body: data}
			}
		}
		if !retryable(last) || attempt == p.options.Retries {
			return last
		}
		if p.options.RetryBackoff > 0 {
			select {
			case <-ctx.Done():
				return upstreamResponse{target: target, err: ctx.Err()}
			case <-time.After(p.options.RetryBackoff):
			}
		}
	}
	return last
}

func (p *Proxy) requestTargets(ctx context.Context, targets []Target, request *http.Request, body []byte) []upstreamResponse {
	responses := make([]upstreamResponse, len(targets))
	var wg sync.WaitGroup
	for i, target := range targets {
		i, target := i, target
		wg.Add(1)
		go func() { defer wg.Done(); responses[i] = p.requestTarget(ctx, target, request, body) }()
	}
	wg.Wait()
	return responses
}

func newRequest(ctx context.Context, target Target, original *http.Request, targetURL *url.URL, body []byte) (*http.Request, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	request, err := http.NewRequestWithContext(ctx, original.Method, targetURL.String(), reader) // #nosec G704 -- target URL is configured.
	if err != nil {
		return nil, err
	}
	for key, values := range original.Header {
		for _, value := range values {
			request.Header.Add(key, value)
		}
	}
	request.Header.Del("Authorization")
	request.Header.Del("Accept-Encoding")
	request.Host = target.Host.Host
	return request, nil
}

func buildURL(host, requestURL *url.URL) *url.URL {
	result := *host
	result.Path = strings.TrimSuffix(host.Path, "/") + "/" + strings.TrimPrefix(requestURL.Path, "/")
	result.RawQuery = requestURL.RawQuery
	return &result
}

func retryable(response upstreamResponse) bool {
	if response.err != nil {
		return true
	}
	return response.status == 429 || response.status == 500 || response.status == 502 || response.status == 503 || response.status == 504
}

func readBody(body io.Reader, limit int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, io.ErrShortBuffer
	}
	return data, nil
}
