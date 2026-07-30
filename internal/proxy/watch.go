package proxy

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"

	appsv1 "k8s.io/api/apps/v1"
)

type watchStream struct {
	target   Target
	response *http.Response
	cancel   context.CancelFunc
}

type deploymentRolloutState struct {
	target     Target
	deployment appsv1.Deployment
}

type deploymentWatchEvent struct {
	target Target
	line   []byte
}

func (p *Proxy) aggregateWatch(w http.ResponseWriter, r *http.Request) {
	targets := p.targets
	resource := parseResourcePath(r.URL.Path)
	if isDeploymentRolloutWatch(r, resource) {
		p.aggregateDeploymentRolloutWatch(w, r, resource)
		return
	}
	if resource.isCollection && r.URL.Query().Get("resourceVersion") != "" && aggregateResourceVersions(r.URL.Query().Get("resourceVersion")) == nil {
		versions, failure := p.collectionWatchResourceVersions(r.Context(), r)
		if failure.err != nil || failure.status != 0 {
			writeTargetFailure(w, failure)
			return
		}
		r = withAggregateResourceVersions(r, versions)
	}
	if resource.isObject && resource.subresource == "" {
		responses := p.probe(r.Context(), r, resource.ownerPath)
		var err error
		targets, err = foundTargets(responses)
		if err != nil {
			writeStatus(w, http.StatusBadGateway, err.Error())
			return
		}
		if len(targets) == 0 {
			writeTargetFailure(w, firstFailure(responses))
			return
		}
		if r.URL.Query().Get("resourceVersion") != "" {
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
			r = withAggregateResourceVersions(r, versions)
		}
	}

	streams, failure := p.openWatches(r.Context(), r, targets)
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

func isDeploymentRolloutWatch(r *http.Request, resource resourcePath) bool {
	if resource.name != "deployments" || resource.subresource != "" {
		return false
	}
	return resource.isObject || resource.isCollection && hasNamedFieldSelector(r.URL.Query().Get("fieldSelector"))
}

func hasNamedFieldSelector(value string) bool {
	for _, selector := range strings.Split(value, ",") {
		key, name, ok := strings.Cut(selector, "=")
		if ok && strings.TrimSpace(key) == "metadata.name" && strings.TrimLeft(name, "=") != "" {
			return true
		}
	}
	return false
}

func (p *Proxy) aggregateDeploymentRolloutWatch(w http.ResponseWriter, r *http.Request, resource resourcePath) {
	targets, states, versions, failure := p.deploymentRolloutTargets(r.Context(), r, resource)
	if failure.err != nil || failure.status != 0 {
		writeTargetFailure(w, failure)
		return
	}
	if len(targets) == 0 {
		writeStatus(w, http.StatusNotFound, "deployment was not found in any configured context")
		return
	}
	if r.URL.Query().Get("resourceVersion") != "" {
		r = withAggregateResourceVersions(r, versions)
	}

	streams, failure := p.openWatches(r.Context(), r, targets)
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
	p.forwardDeploymentRolloutEvents(w, r.Context(), streams, states)
}

func (p *Proxy) deploymentRolloutTargets(ctx context.Context, r *http.Request, resource resourcePath) ([]Target, map[string]deploymentRolloutState, map[string]string, upstreamResponse) {
	if resource.isObject {
		responses := p.probe(ctx, r, resource.ownerPath)
		return deploymentTargetsFromObjects(responses)
	}
	responses := p.requestTargets(ctx, p.targets, collectionWatchVersionRequest(r), nil)
	return deploymentTargetsFromLists(responses)
}

func deploymentTargetsFromObjects(responses []upstreamResponse) ([]Target, map[string]deploymentRolloutState, map[string]string, upstreamResponse) {
	targets := make([]Target, 0, len(responses))
	states := make(map[string]deploymentRolloutState, len(responses))
	versions := make(map[string]string, len(responses))
	for _, response := range responses {
		if response.err != nil || response.status != http.StatusNotFound && (response.status < 200 || response.status >= 300) {
			return nil, nil, nil, response
		}
		if response.status == http.StatusNotFound {
			continue
		}
		var deployment appsv1.Deployment
		if err := json.Unmarshal(response.body, &deployment); err != nil {
			return nil, nil, nil, upstreamResponse{target: response.target, err: err}
		}
		targets = append(targets, response.target)
		states[response.target.Name] = deploymentRolloutState{target: response.target, deployment: deployment}
		versions[response.target.Name] = deployment.ResourceVersion
	}
	return targets, states, versions, upstreamResponse{}
}

func deploymentTargetsFromLists(responses []upstreamResponse) ([]Target, map[string]deploymentRolloutState, map[string]string, upstreamResponse) {
	targets := make([]Target, 0, len(responses))
	states := make(map[string]deploymentRolloutState, len(responses))
	versions := make(map[string]string, len(responses))
	for _, response := range responses {
		if response.err != nil || response.status < 200 || response.status >= 300 {
			return nil, nil, nil, response
		}
		var list appsv1.DeploymentList
		if err := json.Unmarshal(response.body, &list); err != nil {
			return nil, nil, nil, upstreamResponse{target: response.target, err: err}
		}
		if len(list.Items) == 0 {
			continue
		}
		targets = append(targets, response.target)
		states[response.target.Name] = deploymentRolloutState{target: response.target, deployment: list.Items[0]}
		versions[response.target.Name] = list.ResourceVersion
	}
	return targets, states, versions, upstreamResponse{}
}

func (p *Proxy) forwardDeploymentRolloutEvents(w http.ResponseWriter, ctx context.Context, streams []watchStream, states map[string]deploymentRolloutState) {
	events := make(chan deploymentWatchEvent)
	var wg sync.WaitGroup
	for _, stream := range streams {
		stream := stream
		wg.Add(1)
		go func() {
			defer wg.Done()
			readDeploymentWatchEvents(ctx, stream, events)
		}()
	}
	go func() {
		wg.Wait()
		close(events)
	}()

	for event := range events {
		var payload struct {
			Type   string          `json:"type"`
			Object json.RawMessage `json:"object"`
		}
		if json.Unmarshal(event.line, &payload) != nil {
			writeMarkedWatchEvent(w, event.line, event.target.Name)
			continue
		}
		if payload.Type == "ERROR" || payload.Type == "DELETED" {
			writeMarkedWatchEvent(w, event.line, event.target.Name)
			continue
		}
		var deployment appsv1.Deployment
		if json.Unmarshal(payload.Object, &deployment) != nil || deployment.Name == "" {
			continue
		}
		states[event.target.Name] = deploymentRolloutState{target: event.target, deployment: deployment}
		state := states[event.target.Name]
		if incomplete, ok := firstIncompleteDeployment(states); ok {
			state = incomplete
		}
		writeDeploymentWatchEvent(w, state)
	}
}

func readDeploymentWatchEvents(ctx context.Context, stream watchStream, events chan<- deploymentWatchEvent) {
	reader := bufio.NewReader(stream.response.Body)
	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			select {
			case events <- deploymentWatchEvent{target: stream.target, line: line}:
			case <-ctx.Done():
				return
			}
		}
		if err != nil || ctx.Err() != nil {
			return
		}
	}
}

func firstIncompleteDeployment(states map[string]deploymentRolloutState) (deploymentRolloutState, bool) {
	for _, state := range states {
		if !isDeploymentComplete(state.deployment) {
			return state, true
		}
	}
	return deploymentRolloutState{}, false
}

func isDeploymentComplete(deployment appsv1.Deployment) bool {
	if deployment.Generation > deployment.Status.ObservedGeneration {
		return false
	}
	if deployment.Spec.Replicas != nil && deployment.Status.UpdatedReplicas < *deployment.Spec.Replicas {
		return false
	}
	return deployment.Status.Replicas <= deployment.Status.UpdatedReplicas && deployment.Status.AvailableReplicas >= deployment.Status.UpdatedReplicas
}

func writeDeploymentWatchEvent(w http.ResponseWriter, state deploymentRolloutState) {
	payload, err := json.Marshal(map[string]any{"type": "MODIFIED", "object": state.deployment})
	if err != nil {
		return
	}
	writeMarkedWatchEvent(w, append(payload, '\n'), state.target.Name)
}

func writeMarkedWatchEvent(w http.ResponseWriter, line []byte, contextName string) {
	_, _ = w.Write(markEvent(line, contextName))
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (p *Proxy) collectionWatchResourceVersions(ctx context.Context, original *http.Request) (map[string]string, upstreamResponse) {
	responses := p.requestTargets(ctx, p.targets, collectionWatchVersionRequest(original), nil)
	versions := make(map[string]string, len(responses))
	for _, response := range responses {
		if response.err != nil || response.status < 200 || response.status >= 300 {
			return nil, response
		}
		version, err := listResourceVersion(response.body)
		if err != nil {
			return nil, upstreamResponse{target: response.target, err: err}
		}
		versions[response.target.Name] = version
	}
	return versions, upstreamResponse{}
}

func collectionWatchVersionRequest(original *http.Request) *http.Request {
	request := original.Clone(original.Context())
	url := *original.URL
	query := url.Query()
	query.Set("limit", "1")
	query.Del("watch")
	query.Del("resourceVersion")
	query.Del("resourceVersionMatch")
	query.Del("continue")
	url.RawQuery = query.Encode()
	request.URL = &url
	return request
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

func (p *Proxy) openWatches(ctx context.Context, original *http.Request, targets []Target) ([]watchStream, upstreamResponse) {
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
