package relay

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"strings"
)

func parseTokenPath(path, prefix string) (token string, forwardPath string, ok bool) {
	rest := strings.TrimPrefix(path, prefix)
	if rest == path || rest == "" {
		return "", "", false
	}
	token, suffix, _ := strings.Cut(rest, "/")
	if token == "" {
		return "", "", false
	}
	if suffix == "" {
		return token, "/", true
	}
	return token, "/" + suffix, true
}

func cloneHeader(header http.Header) map[string][]string {
	clone := make(map[string][]string, len(header))
	for key, values := range header {
		clone[key] = append([]string(nil), values...)
	}
	return clone
}

func newID(prefix string) (string, error) {
	var bytes [16]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		return "", err
	}
	return prefix + hex.EncodeToString(bytes[:]), nil
}

func acceptsHTML(r *http.Request) bool {
	return strings.Contains(r.Header.Get("Accept"), "text/html")
}
