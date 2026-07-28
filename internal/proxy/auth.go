package proxy

import (
	"crypto/subtle"
	"net/http"
	"strings"
)

// AuthorizedWithToken reports whether r carries a matching "Bearer <token>" Authorization header.
func AuthorizedWithToken(r *http.Request, token string) bool {
	const prefix = "Bearer "

	header := r.Header.Get("Authorization")
	if !strings.HasPrefix(header, prefix) {
		return false
	}

	got := strings.TrimPrefix(header, prefix)
	return subtle.ConstantTimeCompare([]byte(got), []byte(token)) == 1
}
