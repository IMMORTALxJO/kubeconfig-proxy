package proxy

import (
	"encoding/json"
	"net/http"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func writeUpstreamResponse(w http.ResponseWriter, response upstreamResponse) {
	copyHeaders(w.Header(), response.header)
	w.WriteHeader(response.status)
	_, _ = w.Write(response.body)
}

func writeStatusError(w http.ResponseWriter, code int, message string) {
	statusCode := int32(http.StatusInternalServerError)
	if code >= 100 && code <= 599 {
		statusCode = int32(code)
	}
	status := metav1.Status{
		TypeMeta: metav1.TypeMeta{Kind: "Status", APIVersion: "v1"},
		Status:   metav1.StatusFailure,
		Code:     statusCode,
		Message:  message,
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(status)
}

func copyHeaders(dst, src http.Header) {
	for key, values := range src {
		if isHopByHopHeader(key) {
			continue
		}
		dst.Del(key)
		for _, value := range values {
			dst.Add(key, value)
		}
	}
}

func isHopByHopHeader(key string) bool {
	switch strings.ToLower(key) {
	case "connection", "content-length", "keep-alive", "proxy-authenticate", "proxy-authorization", "te", "trailer", "transfer-encoding", "upgrade":
		return true
	default:
		return false
	}
}
