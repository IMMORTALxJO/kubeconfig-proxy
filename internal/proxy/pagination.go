package proxy

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
)

type aggregateContinueToken struct {
	Targets          []string          `json:"targets"`
	Target           string            `json:"target"`
	Request          string            `json:"request"`
	Continue         string            `json:"continue,omitempty"`
	ResourceVersions map[string]string `json:"resourceVersions,omitempty"`
}

type listPageInfo struct {
	arrayKey        string
	itemCount       int
	continueToken   string
	resourceVersion string
}

type paginatedListState struct {
	targetNames      []string
	requestScope     string
	resourceVersions map[string]string
	responses        []upstreamResponse
	limit            int
	itemCount        int
	arrayKey         string
	nextCursor       string
	upstreamContinue string
}

func (p *Proxy) aggregatePaginatedList(w http.ResponseWriter, r *http.Request) {
	state, startIndex, err := p.newPaginatedListState(r)
	if err != nil {
		writeStatusError(w, http.StatusBadRequest, err.Error())
		return
	}
	if !p.collectPaginatedResponses(w, r, startIndex, state) {
		return
	}
	writePaginatedListResponse(w, state)
}

func (p *Proxy) newPaginatedListState(r *http.Request) (*paginatedListState, int, error) {
	limit, err := parseListLimit(r.URL.Query().Get("limit"))
	if err != nil {
		return nil, 0, err
	}

	targetNames := make([]string, 0, len(p.targets))
	for _, target := range p.targets {
		targetNames = append(targetNames, target.Name)
	}
	requestScope := listCursorScope(r)
	startIndex, upstreamContinue, resourceVersions, err := decodeListCursor(r.URL.Query().Get(aggregateContinueQueryKey), targetNames, requestScope)
	if err != nil {
		return nil, 0, err
	}
	return &paginatedListState{
		targetNames:      targetNames,
		requestScope:     requestScope,
		resourceVersions: resourceVersions,
		responses:        make([]upstreamResponse, 0, len(p.targets)-startIndex),
		limit:            limit,
		upstreamContinue: upstreamContinue,
	}, startIndex, nil
}

func (p *Proxy) collectPaginatedResponses(w http.ResponseWriter, r *http.Request, startIndex int, state *paginatedListState) bool {
	for i := startIndex; i < len(p.targets); i++ {
		response, page, ok := p.fetchPaginatedPage(w, r, p.targets[i], state)
		if !ok {
			return false
		}
		if err := state.addPage(response, page, i, len(p.targets)); err != nil {
			writeStatusError(w, http.StatusBadGateway, err.Error())
			return false
		}
		if state.nextCursor != "" {
			break
		}
	}
	return true
}

func (p *Proxy) fetchPaginatedPage(w http.ResponseWriter, r *http.Request, target Target, state *paginatedListState) (upstreamResponse, listPageInfo, bool) {
	request := paginatedRequestForTarget(r, state.remaining(), state.upstreamContinue)
	response := p.do(r.Context(), target, request, nil)
	if response.err != nil {
		writeStatusError(w, http.StatusBadGateway, fmt.Sprintf("%s: %v", response.target.Name, response.err))
		return upstreamResponse{}, listPageInfo{}, false
	}
	if response.status < 200 || response.status >= 300 {
		writeUpstreamResponse(w, response)
		return upstreamResponse{}, listPageInfo{}, false
	}
	page, err := inspectListPage(response.body)
	if err != nil {
		writeStatusError(w, http.StatusBadGateway, fmt.Sprintf("%s: %v", response.target.Name, err))
		return upstreamResponse{}, listPageInfo{}, false
	}
	return response, page, true
}

func (state *paginatedListState) remaining() int {
	if state.limit == 0 {
		return 0
	}
	return state.limit - state.itemCount
}

func (state *paginatedListState) addPage(response upstreamResponse, page listPageInfo, targetIndex, targetCount int) error {
	if state.arrayKey == "" {
		state.arrayKey = page.arrayKey
	} else if page.arrayKey != state.arrayKey {
		return fmt.Errorf("%s: list response field %q does not match %q", response.target.Name, page.arrayKey, state.arrayKey)
	}
	if state.limit > 0 && page.itemCount > state.remaining() {
		return fmt.Errorf("%s: upstream returned %d items for remaining limit %d", response.target.Name, page.itemCount, state.remaining())
	}

	state.responses = append(state.responses, response)
	state.itemCount += page.itemCount
	if page.resourceVersion != "" {
		state.resourceVersions[response.target.Name] = page.resourceVersion
	}
	state.nextCursor = state.cursorAfterPage(response.target.Name, page.continueToken, targetIndex, targetCount)
	state.upstreamContinue = ""
	return nil
}

func (state *paginatedListState) cursorAfterPage(targetName, upstreamContinue string, targetIndex, targetCount int) string {
	if upstreamContinue != "" {
		return encodeListCursor(state.targetNames, targetName, upstreamContinue, state.resourceVersions, state.requestScope)
	}
	if state.limit > 0 && state.itemCount == state.limit && targetIndex+1 < targetCount {
		return encodeListCursor(state.targetNames, state.targetNames[targetIndex+1], "", state.resourceVersions, state.requestScope)
	}
	return ""
}

func writePaginatedListResponse(w http.ResponseWriter, state *paginatedListState) {
	merged, err := mergeListsWithResourceVersions(state.responses, state.resourceVersions)
	if err != nil {
		writeStatusError(w, http.StatusBadGateway, err.Error())
		return
	}
	merged, err = setAggregateContinue(merged, state.nextCursor)
	if err != nil {
		writeStatusError(w, http.StatusBadGateway, err.Error())
		return
	}
	copyHeaders(w.Header(), state.responses[0].header)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(merged) // #nosec G705 -- response body is Kubernetes API JSON, not browser-rendered HTML.
}

func parseListLimit(value string) (int, error) {
	if value == "" {
		return 0, nil
	}
	limit, err := strconv.ParseInt(value, 10, 32)
	if err != nil || limit < 0 {
		return 0, fmt.Errorf("invalid list limit %q", value)
	}
	return int(limit), nil
}

func decodeListCursor(value string, targetNames []string, requestScope string) (int, string, map[string]string, error) {
	if value == "" {
		return 0, "", map[string]string{}, nil
	}
	if !strings.HasPrefix(value, aggregateContinuePrefix) {
		return 0, "", nil, fmt.Errorf("invalid aggregate continue token")
	}
	encoded := strings.TrimPrefix(value, aggregateContinuePrefix)
	payload, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return 0, "", nil, fmt.Errorf("decode aggregate continue token: %w", err)
	}
	var token aggregateContinueToken
	if err := json.Unmarshal(payload, &token); err != nil {
		return 0, "", nil, fmt.Errorf("decode aggregate continue token: %w", err)
	}
	if !slices.Equal(token.Targets, targetNames) {
		return 0, "", nil, fmt.Errorf("aggregate continue token does not match configured targets")
	}
	if token.Request != requestScope {
		return 0, "", nil, fmt.Errorf("aggregate continue token does not match request")
	}
	targetIndex := slices.Index(targetNames, token.Target)
	if targetIndex < 0 {
		return 0, "", nil, fmt.Errorf("aggregate continue token target %q is not configured", token.Target)
	}
	for targetName := range token.ResourceVersions {
		if !slices.Contains(targetNames, targetName) {
			return 0, "", nil, fmt.Errorf("aggregate continue token resource version target %q is not configured", targetName)
		}
	}
	return targetIndex, token.Continue, cloneStringMap(token.ResourceVersions), nil
}

func encodeListCursor(targetNames []string, targetName, upstreamContinue string, resourceVersions map[string]string, requestScope string) string {
	payload, _ := json.Marshal(aggregateContinueToken{
		Targets:          append([]string(nil), targetNames...),
		Target:           targetName,
		Request:          requestScope,
		Continue:         upstreamContinue,
		ResourceVersions: cloneStringMap(resourceVersions),
	})
	return aggregateContinuePrefix + base64.RawURLEncoding.EncodeToString(payload)
}

func listCursorScope(r *http.Request) string {
	query := r.URL.Query()
	query.Del("limit")
	query.Del(aggregateContinueQueryKey)
	return r.URL.Path + "?" + query.Encode()
}

func paginatedRequestForTarget(original *http.Request, limit int, upstreamContinue string) *http.Request {
	request := original.Clone(original.Context())
	requestURL := *original.URL
	query := requestURL.Query()
	if limit > 0 {
		query.Set("limit", strconv.Itoa(limit))
	} else {
		query.Del("limit")
	}
	if upstreamContinue != "" {
		query.Set(aggregateContinueQueryKey, upstreamContinue)
	} else {
		query.Del(aggregateContinueQueryKey)
	}
	requestURL.RawQuery = query.Encode()
	request.URL = &requestURL
	return request
}

func inspectListPage(body []byte) (listPageInfo, error) {
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return listPageInfo{}, fmt.Errorf("decode list response: %w", err)
	}
	page := listPageInfo{resourceVersion: payloadResourceVersion(payload)}
	switch {
	case hasArray(payload, "items"):
		page.arrayKey = "items"
	case hasArray(payload, "rows"):
		page.arrayKey = "rows"
	default:
		return listPageInfo{}, fmt.Errorf("response is not a Kubernetes list or table")
	}
	entries, _ := payload[page.arrayKey].([]any)
	page.itemCount = len(entries)
	if metadata, ok := payload["metadata"].(map[string]any); ok {
		page.continueToken, _ = metadata[aggregateContinueQueryKey].(string)
	}
	return page, nil
}

func setAggregateContinue(body []byte, continueToken string) ([]byte, error) {
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("decode merged list response: %w", err)
	}
	metadata := ensureMap(payload, "metadata")
	if continueToken == "" {
		delete(metadata, aggregateContinueQueryKey)
	} else {
		metadata[aggregateContinueQueryKey] = continueToken
	}
	delete(metadata, "remainingItemCount")
	return json.Marshal(payload)
}
