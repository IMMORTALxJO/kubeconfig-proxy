package proxy

import (
	"net/http"
	"strings"
)

type routeClass uint8

const (
	routePrimary routeClass = iota
	routeWatch
	routePodStream
	routeNamedGet
	routeList
	routeMutation
)

type resourcePath struct {
	isResource   bool
	isCollection bool
	isObject     bool
	isPod        bool
	subresource  string
	ownerPath    string
}

func classifyRequest(r *http.Request, helmMode bool) routeClass {
	if isPrimaryRequest(r, helmMode) {
		return routePrimary
	}
	resource := parseResourcePath(r.URL.Path)
	if isWatch(r) && resource.isResource {
		return routeWatch
	}
	if r.Method == http.MethodGet && resource.isPod && isPodConnection(resource.subresource) {
		return routePodStream
	}
	if r.Method == http.MethodGet && resource.isObject && resource.subresource == "" {
		return routeNamedGet
	}
	if r.Method == http.MethodGet && resource.isCollection {
		return routeList
	}
	if isMutation(r.Method) && resource.isResource {
		return routeMutation
	}
	return routePrimary
}

func isPrimaryRequest(r *http.Request, helmMode bool) bool {
	if isDiscoveryPath(r.URL.Path) || isRequestResponseAPI(r) {
		return true
	}
	return helmMode && isHelmStorageList(r)
}

func parseResourcePath(path string) resourcePath {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	base := 0
	switch {
	case len(parts) >= 3 && parts[0] == "api":
		base = 3
		if len(parts) >= 5 && parts[2] == "namespaces" {
			base = 5
		}
	case len(parts) >= 4 && parts[0] == "apis":
		base = 4
		if len(parts) >= 6 && parts[3] == "namespaces" {
			base = 6
		}
	default:
		return resourcePath{}
	}
	if len(parts) < base {
		return resourcePath{}
	}
	result := resourcePath{isResource: true, isCollection: len(parts) == base}
	if len(parts) > base {
		result.isObject = true
		result.ownerPath = "/" + strings.Join(parts[:base+1], "/")
	}
	if len(parts) > base+1 {
		result.subresource = parts[base+1]
	}
	result.isPod = parts[base-1] == "pods"
	return result
}

func isMutation(method string) bool {
	return method == http.MethodPost || method == http.MethodPut || method == http.MethodPatch || method == http.MethodDelete
}

func isWatch(r *http.Request) bool {
	return r.Method == http.MethodGet && r.URL.Query().Get("watch") == "true"
}

func isPodConnection(subresource string) bool {
	return subresource == "log" || subresource == "exec" || subresource == "attach" || subresource == "portforward"
}

func isRequestResponseAPI(r *http.Request) bool {
	if r.Method != http.MethodPost {
		return false
	}
	path := r.URL.Path
	return strings.Contains(path, "selfsubjectreview") || strings.Contains(path, "accessreview") || strings.Contains(path, "tokenreviews") || strings.HasSuffix(path, "/serviceaccounts/token") || strings.Contains(path, "/serviceaccounts/") && strings.HasSuffix(path, "/token")
}

func isHelmStorageList(r *http.Request) bool {
	if r.Method != http.MethodGet || !parseResourcePath(r.URL.Path).isCollection {
		return false
	}
	path := r.URL.Path
	if !strings.HasSuffix(path, "/secrets") && !strings.HasSuffix(path, "/configmaps") {
		return false
	}
	return strings.Contains(r.URL.Query().Get("labelSelector"), "owner=helm") || strings.Contains(r.URL.Query().Get("labelSelector"), "owner==helm")
}

func isDiscoveryPath(path string) bool {
	trimmed := strings.Trim(path, "/")
	parts := strings.Split(trimmed, "/")
	return path == "/api" || path == "/apis" || path == "/version" ||
		strings.HasPrefix(path, "/openapi") || strings.HasPrefix(path, "/swagger") ||
		strings.HasPrefix(path, "/healthz") || strings.HasPrefix(path, "/livez") || strings.HasPrefix(path, "/readyz") ||
		len(parts) <= 2 && strings.HasPrefix(path, "/api/") || len(parts) <= 3 && strings.HasPrefix(path, "/apis/")
}
