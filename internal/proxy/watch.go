package proxy

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"sync"
	"time"
)

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

func (p *Proxy) aggregateWatch(w http.ResponseWriter, r *http.Request) {
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
	request, err := newUpstreamRequest(requestCtx, target, original, upstreamURL, http.NoBody)
	if err != nil {
		cancel()
		if timer != nil {
			timer.Stop()
		}
		return failedWatchOpen(target, err)
	}
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
		body, readErr := readLimitedBody(response.Body, maxUpstreamResponseBodyBytes, "upstream watch error body")
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
