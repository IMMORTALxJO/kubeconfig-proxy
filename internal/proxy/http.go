package proxy

import (
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func AuthorizedWithToken(r *http.Request, token string) bool {
	value := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	return strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") && subtle.ConstantTimeCompare([]byte(value), []byte(token)) == 1
}

func writeStatus(w http.ResponseWriter, code int, message string) {
	statusCode := int32(http.StatusInternalServerError)
	if code >= http.StatusContinue && code <= 599 {
		statusCode = int32(code)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(metav1.Status{TypeMeta: metav1.TypeMeta{Kind: "Status", APIVersion: "v1"}, Status: metav1.StatusFailure, Code: statusCode, Message: message})
}

func writeResponse(w http.ResponseWriter, response upstreamResponse) {
	for key, values := range response.header {
		if isHopHeader(key) {
			continue
		}
		w.Header().Del(key)
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}
	w.WriteHeader(response.status)
	_, _ = w.Write(response.body)
}

func writeTargetFailure(w http.ResponseWriter, response upstreamResponse) {
	if response.err != nil {
		writeStatus(w, http.StatusBadGateway, response.target.Name+": "+response.err.Error())
		return
	}
	writeStatus(w, response.status, response.target.Name+": upstream returned HTTP "+http.StatusText(response.status))
}

func isHopHeader(key string) bool {
	switch strings.ToLower(key) {
	case "connection", "content-length", "keep-alive", "proxy-authenticate", "proxy-authorization", "te", "trailer", "transfer-encoding", "upgrade":
		return true
	}
	return false
}
