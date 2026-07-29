package proxy

import (
	"context"
	"net/http"
	"net/http/httputil"
)

func (p *Proxy) forwardLongRunning(w http.ResponseWriter, r *http.Request) {
	target := p.primary
	if objectPath, ok := podObjectPathForSubresource(r.URL.Path); ok {
		if found, foundOK := p.findTargetForExistingObject(r.Context(), r, objectPath); foundOK {
			target = found
		}
	}
	p.streamSingle(w, r, target)
}

func (p *Proxy) forwardExistingObject(w http.ResponseWriter, r *http.Request) {
	target := p.primary
	if found, ok := p.findTargetForExistingObject(r.Context(), r, r.URL.Path); ok {
		target = found
	}
	p.forwardSingle(w, r, target)
}

func (p *Proxy) forwardSingle(w http.ResponseWriter, r *http.Request, target Target) {
	if isLongRunningRequest(r) {
		p.streamSingle(w, r, target)
		return
	}

	response := p.requestTarget(r.Context(), target, r, nil)
	if response.err != nil {
		writeStatusError(w, http.StatusBadGateway, response.err.Error())
		return
	}
	writeUpstreamResponse(w, response)
}

func (p *Proxy) findTargetForExistingObject(ctx context.Context, original *http.Request, objectPath string) (Target, bool) {
	request := newExistingObjectRequest(ctx, original, objectPath)
	for _, target := range p.targets {
		response := p.requestTarget(ctx, target, request, nil)
		if response.err == nil && response.status >= 200 && response.status < 300 {
			return response.target, true
		}
	}
	return Target{}, false
}

func newExistingObjectRequest(ctx context.Context, original *http.Request, objectPath string) *http.Request {
	objectURL := *original.URL
	objectURL.Path = objectPath
	objectURL.RawQuery = ""

	request := original.Clone(ctx)
	request.Method = http.MethodGet
	request.URL = &objectURL
	request.Body = nil
	request.ContentLength = 0
	return request
}

func (*Proxy) streamSingle(w http.ResponseWriter, r *http.Request, target Target) {
	reverseProxy := &httputil.ReverseProxy{
		Transport:     target.Client.Transport,
		FlushInterval: -1,
		Rewrite: func(proxyRequest *httputil.ProxyRequest) {
			upstreamURL := buildUpstreamURL(target.Host, proxyRequest.In.URL)
			proxyRequest.Out.URL = upstreamURL
			proxyRequest.Out.Host = target.Host.Host
			proxyRequest.Out.Header.Del("Authorization")
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			writeStatusError(w, http.StatusBadGateway, err.Error())
		},
	}
	reverseProxy.ServeHTTP(w, r)
}
