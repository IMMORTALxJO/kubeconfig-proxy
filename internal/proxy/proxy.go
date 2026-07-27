package proxy

import (
	"fmt"
	"net/http"
	"time"
)

const (
	DefaultRetries                   = 5
	contextNameAnnotation            = "kubeconfig-proxy.io/context-name"
	singleContextAnnotation          = "kubeconfig-proxy.io/single-context"
	sourceContextAnnotation          = "kubeconfig-proxy.io/context"
	sourceContextLabel               = "context"
	aggregateResourceVersionPrefix   = "kubeconfig-proxy:"
	aggregateResourceVersionQueryKey = "resourceVersion"
)

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

type Options struct {
	RequestTimeout   time.Duration
	Retries          int
	RetryBackoff     time.Duration
	BearerToken      string
	HelmReleaseProxy bool
	ReadOnly         bool
}

func NewWithOptions(targets []Target, primary Target, options Options) (*Proxy, error) {
	if len(targets) == 0 {
		return nil, fmt.Errorf("at least one target is required")
	}
	if options.Retries < 0 {
		return nil, fmt.Errorf("retries must be greater than or equal to 0")
	}
	if options.RequestTimeout < 0 {
		return nil, fmt.Errorf("request timeout must be greater than or equal to 0")
	}
	if options.RetryBackoff < 0 {
		return nil, fmt.Errorf("retry backoff must be greater than or equal to 0")
	}
	if options.BearerToken == "" {
		return nil, fmt.Errorf("bearer token is required")
	}
	return &Proxy{targets: targets, primary: primary, options: options}, nil
}

func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !p.authorized(r) {
		writeStatusError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if p.options.ReadOnly && isMutating(r.Method) {
		writeStatusError(w, http.StatusForbidden, "read-only proxy rejects mutating requests")
		return
	}

	switch {
	case isWatchRequest(r) && p.options.HelmReleaseProxy && isHelmStorageListRequest(r):
		p.streamSingle(w, r, p.primary)
	case isWatchRequest(r):
		p.aggregateWatch(w, r)
	case isLongRunningRequest(r):
		p.forwardLongRunning(w, r)
	case p.shouldUsePrimaryOnly(r):
		p.forwardSingle(w, r, p.primary)
	case isNamedResourceGetRequest(r):
		p.forwardExistingObject(w, r)
	case isAggregatableListRequest(r):
		p.aggregateList(w, r)
	case isMutating(r.Method):
		p.fanOut(w, r)
	default:
		p.forwardSingle(w, r, p.primary)
	}
}
