package proxy

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"sync"
)

type watchStream struct {
	target   Target
	response *http.Response
	cancel   context.CancelFunc
}

func (p *Proxy) aggregateWatch(w http.ResponseWriter, r *http.Request) {
	streams, failure := p.openWatches(r.Context(), r)
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

func closeWatchStream(stream watchStream) {
	if stream.response != nil {
		_ = stream.response.Body.Close()
	}
	if stream.cancel != nil {
		stream.cancel()
	}
}

func (p *Proxy) openWatches(ctx context.Context, original *http.Request) ([]watchStream, upstreamResponse) {
	resourceVersions := aggregateResourceVersions(original.URL.Query().Get("resourceVersion"))
	results := make([]watchStream, len(p.targets))
	failures := make([]upstreamResponse, len(p.targets))
	var wg sync.WaitGroup
	for i, target := range p.targets {
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
