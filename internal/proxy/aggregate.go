package proxy

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

const (
	aggregateContinuePrefix        = "kubeconfig-proxy-continue:"
	aggregateResourceVersionPrefix = "kubeconfig-proxy:"
	maxAggregatePageLimit          = 10000
)

type pageCursor struct {
	Target           int               `json:"target"`
	Continue         string            `json:"continue,omitempty"`
	Scope            string            `json:"scope"`
	Targets          string            `json:"targets"`
	ResourceVersions map[string]string `json:"resourceVersions,omitempty"`
}

func (p *Proxy) aggregateList(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("limit") != "" || r.URL.Query().Get("continue") != "" {
		p.aggregatePage(w, r)
		return
	}
	responses := p.requestTargets(r.Context(), p.targets, r, nil)
	for _, response := range responses {
		if response.err != nil || response.status < 200 || response.status >= 300 {
			writeTargetFailure(w, response)
			return
		}
	}
	body, err := mergeLists(responses)
	if err != nil {
		writeStatus(w, http.StatusBadGateway, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

func (p *Proxy) aggregatePage(w http.ResponseWriter, r *http.Request) {
	limit, err := aggregatePageLimit(r)
	if err != nil {
		writeStatus(w, http.StatusBadRequest, "invalid list limit")
		return
	}
	if limit == 0 {
		p.aggregateListWithoutPage(w, r)
		return
	}
	cursor, err := p.decodeCursor(r)
	if err != nil {
		writeStatus(w, http.StatusBadRequest, err.Error())
		return
	}
	resourceVersions, failure := p.pageResourceVersions(r, cursor)
	if failure.err != nil || failure.status != 0 {
		writeAggregatePageFailure(w, failure)
		return
	}
	items := make([]any, 0)
	var template map[string]any
	for index := cursor.Target; index < len(p.targets) && len(items) < limit; index++ {
		page, entries, next, failure := p.readAggregatePage(r, index, cursor, limit-len(items), resourceVersions[p.targets[index].Name])
		if failure.err != nil || failure.status != 0 {
			writeAggregatePageFailure(w, failure)
			return
		}
		items = append(items, entries...)
		template = page
		if next != "" {
			setCursor(template, pageCursor{Target: index, Continue: next, Scope: pageScope(r), Targets: p.targetSet(), ResourceVersions: resourceVersions})
			break
		}
		if len(items) == limit && index+1 < len(p.targets) {
			setCursor(template, pageCursor{Target: index + 1, Scope: pageScope(r), Targets: p.targetSet(), ResourceVersions: resourceVersions})
			break
		}
	}
	if template == nil {
		template = map[string]any{}
	}
	if _, entriesKey, ok := listEntries(template); ok {
		template[entriesKey] = items
	} else {
		template["items"] = items
	}
	setAggregateResourceVersionValues(template, resourceVersions)
	result, _ := json.Marshal(template)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(result)
}

func (p *Proxy) pageResourceVersions(r *http.Request, cursor pageCursor) (map[string]string, upstreamResponse) {
	if len(cursor.ResourceVersions) != 0 {
		versions := make(map[string]string, len(p.targets))
		for _, target := range p.targets {
			version := cursor.ResourceVersions[target.Name]
			if version == "" {
				return nil, upstreamResponse{target: target, err: fmt.Errorf("missing page resource version")}
			}
			versions[target.Name] = version
		}
		return versions, upstreamResponse{}
	}

	responses := p.requestTargets(r.Context(), p.targets, aggregatePageVersionRequest(r), nil)
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

func aggregatePageVersionRequest(r *http.Request) *http.Request {
	request := r.Clone(r.Context())
	url := *r.URL
	query := url.Query()
	query.Set("limit", "1")
	query.Del("continue")
	query.Del("resourceVersion")
	query.Del("resourceVersionMatch")
	url.RawQuery = query.Encode()
	request.URL = &url
	return request
}

func listResourceVersion(body []byte) (string, error) {
	var payload struct {
		Metadata struct {
			ResourceVersion string `json:"resourceVersion"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return "", fmt.Errorf("decode list response: %w", err)
	}
	if payload.Metadata.ResourceVersion == "" {
		return "", fmt.Errorf("list response has no resource version")
	}
	return payload.Metadata.ResourceVersion, nil
}

func (p *Proxy) readAggregatePage(r *http.Request, index int, cursor pageCursor, limit int, resourceVersion string) (map[string]any, []any, string, upstreamResponse) {
	request := aggregatePageRequest(r, index == cursor.Target, cursor.Continue, limit, resourceVersion)
	response := p.requestTarget(r.Context(), p.targets[index], request, nil)
	if response.err != nil || response.status < 200 || response.status >= 300 {
		return nil, nil, "", response
	}
	var page map[string]any
	if json.Unmarshal(response.body, &page) != nil {
		return nil, nil, "", upstreamResponse{target: response.target, err: fmt.Errorf("decode list response")}
	}
	entries, _, ok := listEntries(page)
	if !ok {
		return nil, nil, "", upstreamResponse{target: response.target, err: fmt.Errorf("response is not a Kubernetes list or table")}
	}
	for _, entry := range entries {
		markEntry(entry, response.target.Name)
	}
	metadata, _ := page["metadata"].(map[string]any)
	next, _ := metadata["continue"].(string)
	return page, entries, next, upstreamResponse{}
}

func aggregatePageRequest(r *http.Request, isCursorTarget bool, continueToken string, limit int, resourceVersion string) *http.Request {
	request := r.Clone(r.Context())
	url := *r.URL
	query := url.Query()
	query.Set("limit", strconv.Itoa(limit))
	if isCursorTarget && continueToken != "" {
		query.Set("continue", continueToken)
		query.Del("resourceVersion")
		query.Del("resourceVersionMatch")
	} else {
		query.Del("continue")
		query.Set("resourceVersion", resourceVersion)
		query.Set("resourceVersionMatch", "Exact")
	}
	url.RawQuery = query.Encode()
	request.URL = &url
	return request
}

func writeAggregatePageFailure(w http.ResponseWriter, response upstreamResponse) {
	if response.err != nil && response.status == 0 {
		writeStatus(w, http.StatusBadGateway, response.target.Name+": "+response.err.Error())
		return
	}
	writeTargetFailure(w, response)
}

func aggregatePageLimit(r *http.Request) (int, error) {
	limit, err := strconv.Atoi(r.URL.Query().Get("limit"))
	if err != nil || limit < 0 || limit > maxAggregatePageLimit {
		return 0, fmt.Errorf("invalid list limit")
	}
	return limit, nil
}

func (p *Proxy) aggregateListWithoutPage(w http.ResponseWriter, r *http.Request) {
	clone := r.Clone(r.Context())
	url := *r.URL
	query := url.Query()
	query.Del("limit")
	query.Del("continue")
	url.RawQuery = query.Encode()
	clone.URL = &url
	responses := p.requestTargets(r.Context(), p.targets, clone, nil)
	for _, response := range responses {
		if response.err != nil || response.status < 200 || response.status >= 300 {
			writeTargetFailure(w, response)
			return
		}
	}
	body, err := mergeLists(responses)
	if err != nil {
		writeStatus(w, http.StatusBadGateway, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

func pageScope(r *http.Request) string {
	query := r.URL.Query()
	query.Del("limit")
	query.Del("continue")
	return r.URL.Path + "?" + query.Encode()
}
func (p *Proxy) decodeCursor(r *http.Request) (pageCursor, error) {
	value := r.URL.Query().Get("continue")
	if value == "" {
		return pageCursor{Scope: pageScope(r), Targets: p.targetSet()}, nil
	}
	if !strings.HasPrefix(value, aggregateContinuePrefix) {
		return pageCursor{}, fmt.Errorf("invalid aggregate continue token")
	}
	data, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(value, aggregateContinuePrefix))
	if err != nil {
		return pageCursor{}, err
	}
	var cursor pageCursor
	if json.Unmarshal(data, &cursor) != nil || cursor.Scope != pageScope(r) || cursor.Targets != p.targetSet() || cursor.Target < 0 || cursor.Target >= len(p.targets) {
		return pageCursor{}, fmt.Errorf("invalid aggregate continue token")
	}
	return cursor, nil
}

func (p *Proxy) targetSet() string {
	names := make([]string, len(p.targets))
	for index, target := range p.targets {
		names[index] = target.Name
	}
	return strings.Join(names, "\x00")
}
func setCursor(payload map[string]any, cursor pageCursor) {
	data, _ := json.Marshal(cursor)
	metadata, ok := payload["metadata"].(map[string]any)
	if !ok {
		metadata = map[string]any{}
		payload["metadata"] = metadata
	}
	metadata["continue"] = aggregateContinuePrefix + base64.RawURLEncoding.EncodeToString(data)
}

func mergeLists(responses []upstreamResponse) ([]byte, error) {
	var merged map[string]any
	for _, response := range responses {
		var value map[string]any
		if err := json.Unmarshal(response.body, &value); err != nil {
			return nil, fmt.Errorf("%s: decode list response: %w", response.target.Name, err)
		}
		entries, entriesKey, ok := listEntries(value)
		if !ok {
			return nil, fmt.Errorf("%s: response is not a Kubernetes list or table", response.target.Name)
		}
		for _, entry := range entries {
			markEntry(entry, response.target.Name)
		}
		if merged == nil {
			merged = value
			merged[entriesKey] = entries
			continue
		}
		_, mergedEntriesKey, mergedOK := listEntries(merged)
		if !mergedOK || mergedEntriesKey != entriesKey {
			return nil, fmt.Errorf("%s: response kind differs from previous target", response.target.Name)
		}
		current, _ := merged[mergedEntriesKey].([]any)
		merged[mergedEntriesKey] = append(current, entries...)
	}
	if merged != nil {
		setAggregateResourceVersion(merged, responses)
	}
	return json.Marshal(merged)
}

func setAggregateResourceVersion(payload map[string]any, responses []upstreamResponse) {
	versions := map[string]string{}
	for _, response := range responses {
		var value map[string]any
		if json.Unmarshal(response.body, &value) == nil {
			if metadata, ok := value["metadata"].(map[string]any); ok {
				if version, ok := metadata["resourceVersion"].(string); ok {
					versions[response.target.Name] = version
				}
			}
		}
	}
	setAggregateResourceVersionValues(payload, versions)
}

func setAggregateResourceVersionValues(payload map[string]any, versions map[string]string) {
	data, _ := json.Marshal(versions)
	metadata, ok := payload["metadata"].(map[string]any)
	if !ok {
		metadata = map[string]any{}
		payload["metadata"] = metadata
	}
	metadata["resourceVersion"] = aggregateResourceVersionPrefix + base64.RawURLEncoding.EncodeToString(data)
}

func aggregateResourceVersions(value string) map[string]string {
	if !strings.HasPrefix(value, aggregateResourceVersionPrefix) {
		return nil
	}
	data, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(value, aggregateResourceVersionPrefix))
	if err != nil {
		return nil
	}
	versions := map[string]string{}
	if json.Unmarshal(data, &versions) != nil {
		return nil
	}
	return versions
}

func listEntries(payload map[string]any) ([]any, string, bool) {
	if entries, ok := payload["items"].([]any); ok {
		return entries, "items", true
	}
	if entries, ok := payload["rows"].([]any); ok {
		return entries, "rows", true
	}
	return nil, "", false
}

func markEntry(entry any, contextName string) {
	object, ok := entry.(map[string]any)
	if !ok {
		return
	}
	if embedded, ok := object["object"].(map[string]any); ok {
		object = embedded
	}
	metadata, ok := object["metadata"].(map[string]any)
	if !ok {
		metadata = map[string]any{}
		object["metadata"] = metadata
	}
	annotations, ok := metadata["annotations"].(map[string]any)
	if !ok {
		annotations = map[string]any{}
		metadata["annotations"] = annotations
	}
	annotations[sourceContextAnnotation] = contextName
	labels, ok := metadata["labels"].(map[string]any)
	if !ok {
		labels = map[string]any{}
		metadata["labels"] = labels
	}
	labels[sourceContextLabel] = contextName
}
