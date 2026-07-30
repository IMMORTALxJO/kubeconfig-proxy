package proxy

import (
	"fmt"
	"net/http"
	"time"

	"github.com/IMMORTALxJO/kubeconfig-proxy/internal/upstream"
)

const (
	DefaultRetries = 5

	contextNameAnnotation   = "kubeconfig-proxy.io/context-name"
	singleContextAnnotation = "kubeconfig-proxy.io/single-context"
	sourceContextAnnotation = "kubeconfig-proxy.io/context"
	sourceContextLabel      = "context"
)

type Target = upstream.Target

type Options struct {
	RequestTimeout   time.Duration
	Retries          int
	RetryBackoff     time.Duration
	BearerToken      string
	HelmReleaseProxy bool
	ReadOnly         bool
}

type Proxy struct {
	targets []Target
	primary Target
	options Options
}

type upstreamResponse struct {
	target Target
	status int
	header http.Header
	body   []byte
	err    error
}

func NewWithOptions(targets []Target, primary Target, options Options) (*Proxy, error) {
	if len(targets) == 0 || primary.Name == "" || options.BearerToken == "" {
		return nil, fmt.Errorf("proxy requires targets, a primary target, and a bearer token")
	}
	seenPrimary := false
	seen := make(map[string]struct{}, len(targets))
	for _, target := range targets {
		if target.Name == "" || target.Host == nil || target.Client == nil {
			return nil, fmt.Errorf("target %q is incomplete", target.Name)
		}
		if _, ok := seen[target.Name]; ok {
			return nil, fmt.Errorf("target %q is configured more than once", target.Name)
		}
		seen[target.Name] = struct{}{}
		seenPrimary = seenPrimary || target.Name == primary.Name
	}
	if !seenPrimary || options.Retries < 0 || options.RequestTimeout < 0 || options.RetryBackoff < 0 {
		return nil, fmt.Errorf("invalid proxy configuration")
	}
	return &Proxy{targets: targets, primary: primary, options: options}, nil
}

func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !AuthorizedWithToken(r, p.options.BearerToken) {
		writeStatus(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if p.options.ReadOnly && isMutation(r.Method) {
		writeStatus(w, http.StatusForbidden, "read-only proxy rejects mutating requests")
		return
	}

	class := classifyRequest(r, p.options.HelmReleaseProxy)
	switch class {
	case routePrimary:
		p.forwardSingle(w, r, p.primary)
	case routeWatch:
		p.aggregateWatch(w, r)
	case routePodStream:
		p.forwardPodStream(w, r)
	case routeNamedGet:
		p.forwardNamedGet(w, r)
	case routeList:
		p.aggregateList(w, r)
	case routeMutation:
		p.forwardMutation(w, r)
	default:
		p.forwardSingle(w, r, p.primary)
	}
}
