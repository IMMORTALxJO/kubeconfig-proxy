package proxy

import (
	"net/http"
	"strings"
)

func (p *Proxy) shouldUsePrimaryOnly(r *http.Request) bool {
	return isDiscoveryPath(r.URL.Path) || p.options.HelmReleaseProxy && isHelmStorageListRequest(r)
}

func shouldUseRequestTimeout(r *http.Request) bool {
	return !isLongRunningRequest(r)
}

func isLongRunningRequest(r *http.Request) bool {
	if isWatchRequest(r) {
		return true
	}
	_, ok := podObjectPathForSubresource(r.URL.Path)
	return ok
}

func splitPath(path string) []string {
	return strings.Split(strings.Trim(path, "/"), "/")
}

func podObjectPathForSubresource(path string) (string, bool) {
	podPath, ok := podObjectPath(path)
	if !ok {
		return "", false
	}
	parts := splitPath(path)
	switch parts[6] {
	case "attach", "exec", "log", "portforward":
		return podPath, true
	default:
		return "", false
	}
}

func podObjectPath(path string) (string, bool) {
	parts := splitPath(path)
	if len(parts) != 7 || parts[0] != "api" || parts[2] != "namespaces" || parts[4] != "pods" {
		return "", false
	}
	return "/" + strings.Join(parts[:6], "/"), true
}

func isAggregatableListRequest(r *http.Request) bool {
	return r.Method == http.MethodGet && !isDiscoveryPath(r.URL.Path) && r.URL.Query().Get("watch") != "true"
}

func isWatchRequest(r *http.Request) bool {
	return r.Method == http.MethodGet && r.URL.Query().Get("watch") == "true"
}

func isHelmStorageListRequest(r *http.Request) bool {
	if r.Method != http.MethodGet {
		return false
	}
	if !isCoreResourceListPath(r.URL.Path, "secrets", "configmaps") {
		return false
	}
	return labelSelectorHas(r.URL.Query().Get("labelSelector"), "owner", "helm")
}

func isCoreResourceListPath(path string, resources ...string) bool {
	parts := splitPath(path)
	resource := ""
	switch {
	case len(parts) == 3 && parts[0] == "api" && parts[1] == "v1":
		resource = parts[2]
	case len(parts) == 5 && parts[0] == "api" && parts[1] == "v1" && parts[2] == "namespaces":
		resource = parts[4]
	default:
		return false
	}

	for _, candidate := range resources {
		if resource == candidate {
			return true
		}
	}
	return false
}

func labelSelectorHas(selector, key, value string) bool {
	for _, requirement := range strings.Split(selector, ",") {
		requirement = strings.TrimSpace(requirement)
		if requirement == key+"="+value || requirement == key+"=="+value {
			return true
		}
	}
	return false
}

func isNamedFieldSelector(selector string) bool {
	for _, requirement := range strings.Split(selector, ",") {
		requirement = strings.TrimSpace(requirement)
		if strings.HasPrefix(requirement, "metadata.name=") || strings.HasPrefix(requirement, "metadata.name==") {
			return true
		}
	}
	return false
}

func isNamedResourceGetRequest(r *http.Request) bool {
	return r.Method == http.MethodGet && isNamedResourcePath(r.URL.Path)
}

func isNamedResourcePath(path string) bool {
	parts := splitPath(path)
	if len(parts) == 4 && parts[0] == "api" {
		return true
	}
	if len(parts) == 6 && parts[0] == "api" && parts[2] == "namespaces" {
		return true
	}
	if len(parts) == 5 && parts[0] == "apis" {
		return true
	}
	return len(parts) == 7 && parts[0] == "apis" && parts[3] == "namespaces"
}

func isMutating(method string) bool {
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	default:
		return false
	}
}

func isDiscoveryPath(path string) bool {
	return path == "/api" ||
		path == "/apis" ||
		path == "/version" ||
		strings.HasPrefix(path, "/api/") && strings.Count(strings.Trim(path, "/"), "/") <= 1 ||
		strings.HasPrefix(path, "/apis/") && strings.Count(strings.Trim(path, "/"), "/") <= 2 ||
		strings.HasPrefix(path, "/openapi") ||
		strings.HasPrefix(path, "/swagger") ||
		strings.HasPrefix(path, "/healthz") ||
		strings.HasPrefix(path, "/livez") ||
		strings.HasPrefix(path, "/readyz")
}
