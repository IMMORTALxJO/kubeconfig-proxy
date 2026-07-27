package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"sigs.k8s.io/yaml"
)

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

	resource, err := decodeObject(body)
	if err != nil {
		return nil
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
