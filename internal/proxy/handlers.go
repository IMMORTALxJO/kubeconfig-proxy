package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httputil"
	"strings"
	"sync"

	"sigs.k8s.io/yaml"
)

func (p *Proxy) forwardSingle(w http.ResponseWriter, r *http.Request, target Target) {
	response := p.requestTarget(r.Context(), target, r, nil)
	if response.err != nil {
		writeTargetFailure(w, response)
		return
	}
	writeResponse(w, response)
}

func (p *Proxy) forwardNamedGet(w http.ResponseWriter, r *http.Request) {
	responses := p.probe(r.Context(), r, r.URL.Path)
	if response, ok := p.firstSuccess(responses); ok {
		writeResponse(w, response)
		return
	}
	writeTargetFailure(w, firstFailure(responses))
}

func (p *Proxy) forwardPodStream(w http.ResponseWriter, r *http.Request) {
	owner := parseResourcePath(r.URL.Path).ownerPath
	responses := p.probe(r.Context(), r, owner)
	if response, ok := p.firstSuccess(responses); ok {
		stream(w, r, response.target)
		return
	}
	if failure := firstFailure(responses); failure.err != nil || failure.status != http.StatusNotFound {
		writeTargetFailure(w, failure)
		return
	}
	stream(w, r, p.primary)
}

func (p *Proxy) probe(ctx context.Context, original *http.Request, path string) []upstreamResponse {
	request := original.Clone(ctx)
	url := *original.URL
	url.Path, url.RawQuery = path, ""
	request.Method, request.URL, request.Body, request.ContentLength = http.MethodGet, &url, nil, 0
	return p.requestTargets(ctx, p.targets, request, nil)
}

func (p *Proxy) firstSuccess(responses []upstreamResponse) (upstreamResponse, bool) {
	for _, response := range responses {
		if response.target.Name == p.primary.Name && response.err == nil && response.status >= 200 && response.status < 300 {
			return response, true
		}
	}
	for _, response := range responses {
		if response.err == nil && response.status >= 200 && response.status < 300 {
			return response, true
		}
	}
	return upstreamResponse{}, false
}

func firstFailure(responses []upstreamResponse) upstreamResponse {
	for _, response := range responses {
		if response.err != nil || response.status != http.StatusNotFound {
			return response
		}
	}
	return responses[0]
}

func stream(w http.ResponseWriter, r *http.Request, target Target) {
	proxy := &httputil.ReverseProxy{Transport: target.Client.Transport, FlushInterval: -1, Rewrite: func(request *httputil.ProxyRequest) {
		request.Out.URL = buildURL(target.Host, request.In.URL)
		request.Out.Host = target.Host.Host
		request.Out.Header.Del("Authorization")
	}, ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
		writeStatus(w, http.StatusBadGateway, target.Name+": "+err.Error())
	}}
	proxy.ServeHTTP(w, r)
}

func (p *Proxy) forwardMutation(w http.ResponseWriter, r *http.Request) {
	body, err := readBody(r.Body, 16<<20)
	if err != nil {
		writeStatus(w, http.StatusRequestEntityTooLarge, err.Error())
		return
	}
	targets, err := p.mutationTargets(r.Context(), r, body)
	if err != nil {
		writeStatus(w, http.StatusBadRequest, err.Error())
		return
	}
	responses := p.requestMutationTargets(r.Context(), targets, r, body)
	for _, response := range responses {
		if response.err != nil || response.status < 200 || response.status >= 300 {
			writeTargetFailure(w, response)
			return
		}
	}
	for _, response := range responses {
		if response.target.Name == p.primary.Name {
			writeResponse(w, response)
			return
		}
	}
	writeResponse(w, responses[0])
}

func (p *Proxy) requestMutationTargets(ctx context.Context, targets []Target, r *http.Request, body []byte) []upstreamResponse {
	if r.Method != http.MethodPut {
		return p.requestTargets(ctx, targets, r, body)
	}
	responses := make([]upstreamResponse, len(targets))
	var wg sync.WaitGroup
	for i, target := range targets {
		i, target := i, target
		wg.Add(1)
		go func() {
			defer wg.Done()
			get := r.Clone(ctx)
			get.Method, get.Body, get.ContentLength = http.MethodGet, nil, 0
			current := p.requestTarget(ctx, target, get, nil)
			targetBody := body
			if current.err != nil {
				responses[i] = current
				return
			}
			if current.status >= 200 && current.status < 300 {
				if rewritten, err := rewriteIdentity(body, current.body); err != nil {
					responses[i] = upstreamResponse{target: target, err: err}
					return
				} else {
					targetBody = rewritten
				}
			}
			responses[i] = p.requestTarget(ctx, target, r, targetBody)
		}()
	}
	wg.Wait()
	return responses
}

func (p *Proxy) mutationTargets(ctx context.Context, r *http.Request, body []byte) ([]Target, error) {
	resource := parseResourcePath(r.URL.Path)
	if targets, handled, err := p.annotationTargets(annotations(body), false); handled {
		return targets, err
	}
	if !needsExistingObject(r.Method, resource) {
		return p.targets, nil
	}
	responses := p.probe(ctx, r, resource.ownerPath)
	if targets, handled, err := p.existingAnnotationTargets(responses); handled {
		return targets, err
	}
	if targets, err := foundTargets(responses); err != nil {
		return nil, err
	} else if len(targets) > 0 {
		return targets, nil
	}
	return p.targets, nil
}

func needsExistingObject(method string, resource resourcePath) bool {
	return resource.isObject && (method == http.MethodPut || method == http.MethodPatch || method == http.MethodDelete)
}

func (p *Proxy) annotationTargets(values map[string]string, existing bool) ([]Target, bool, error) {
	if name := values[contextNameAnnotation]; name != "" {
		target, ok := p.target(name)
		if !ok {
			prefix := ""
			if existing {
				prefix = " on existing object"
			}
			return nil, true, fmt.Errorf("context %q%s is not configured", name, prefix)
		}
		return []Target{target}, true, nil
	}
	if strings.EqualFold(values[singleContextAnnotation], "true") {
		return []Target{p.primary}, true, nil
	}
	return nil, false, nil
}

func (p *Proxy) existingAnnotationTargets(responses []upstreamResponse) ([]Target, bool, error) {
	for _, response := range responses {
		if response.err != nil || response.status < 200 || response.status >= 300 {
			continue
		}
		if targets, handled, err := p.annotationTargets(annotations(response.body), true); handled {
			return targets, true, err
		}
	}
	return nil, false, nil
}

func foundTargets(responses []upstreamResponse) ([]Target, error) {
	found := make([]Target, 0, len(responses))
	for _, response := range responses {
		if response.err != nil {
			return nil, fmt.Errorf("%s: %w", response.target.Name, response.err)
		}
		if response.status >= 200 && response.status < 300 {
			found = append(found, response.target)
			continue
		}
		if response.status != http.StatusNotFound {
			return nil, fmt.Errorf("%s: existing object returned HTTP %d", response.target.Name, response.status)
		}
	}
	return found, nil
}

func (p *Proxy) target(name string) (Target, bool) {
	for _, target := range p.targets {
		if target.Name == name {
			return target, true
		}
	}
	return Target{}, false
}

func annotations(body []byte) map[string]string {
	var object map[string]any
	if json.Unmarshal(body, &object) != nil {
		jsonBody, err := yaml.YAMLToJSON(body)
		if err != nil || json.Unmarshal(jsonBody, &object) != nil {
			return nil
		}
	}
	metadata, _ := object["metadata"].(map[string]any)
	raw, _ := metadata["annotations"].(map[string]any)
	result := make(map[string]string, len(raw))
	for key, value := range raw {
		if stringValue, ok := value.(string); ok {
			result[key] = stringValue
		}
	}
	return result
}

func rewriteIdentity(body, currentBody []byte) ([]byte, error) {
	var desired, current map[string]any
	if json.Unmarshal(body, &desired) != nil || json.Unmarshal(currentBody, &current) != nil {
		return body, nil
	}
	desiredMetadata, _ := desired["metadata"].(map[string]any)
	currentMetadata, _ := current["metadata"].(map[string]any)
	if desiredMetadata == nil || currentMetadata == nil {
		return body, nil
	}
	for _, key := range []string{"uid", "resourceVersion"} {
		if value, ok := currentMetadata[key]; ok {
			desiredMetadata[key] = value
		}
	}
	return json.Marshal(desired)
}
