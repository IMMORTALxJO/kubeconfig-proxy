package proxy

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

func (p *Proxy) aggregateList(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("limit") != "" || r.URL.Query().Get(aggregateContinueQueryKey) != "" {
		p.aggregatePaginatedList(w, r)
		return
	}

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

func mergeLists(responses []upstreamResponse) ([]byte, error) {
	return mergeListsWithResourceVersions(responses, nil)
}

func mergeListsWithResourceVersions(responses []upstreamResponse, initialResourceVersions map[string]string) ([]byte, error) {
	var merged map[string]any
	resourceVersions := cloneStringMap(initialResourceVersions)
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
			mergeArrayField(payload, &merged, "items", response.target.Name, entryMetadata)
		case hasArray(payload, "rows"):
			mergeArrayField(payload, &merged, "rows", response.target.Name, tableRowMetadata)
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

func cloneStringMap(source map[string]string) map[string]string {
	cloned := make(map[string]string, len(source))
	for key, value := range source {
		cloned[key] = value
	}
	return cloned
}

// entryMetadata returns the metadata map of a list item.
func entryMetadata(entry map[string]any) map[string]any {
	return ensureMap(entry, "metadata")
}

// tableRowMetadata returns the metadata map of a table row's embedded object, if any.
func tableRowMetadata(row map[string]any) map[string]any {
	object, ok := row["object"].(map[string]any)
	if !ok {
		return nil
	}
	return ensureMap(object, "metadata")
}

func mergeArrayField(payload map[string]any, merged *map[string]any, key, contextName string, metadataOf func(map[string]any) map[string]any) {
	entries, _ := payload[key].([]any)
	for i := range entries {
		entry, ok := entries[i].(map[string]any)
		if !ok {
			continue
		}
		if metadata := metadataOf(entry); metadata != nil {
			markSourceContext(metadata, contextName)
		}
	}

	if *merged == nil {
		*merged = payload
		(*merged)[key] = entries
		return
	}

	mergedEntries, _ := (*merged)[key].([]any)
	(*merged)[key] = append(mergedEntries, entries...)
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
