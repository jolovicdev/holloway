package relay

import (
	"crypto/sha256"
	"crypto/subtle"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

func (s *Server) validToken(w http.ResponseWriter, tokenID string) bool {
	exists, err := s.store.TokenExists(tokenID)
	if err != nil {
		http.Error(w, "check token", http.StatusInternalServerError)
		return false
	}
	if !exists {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return false
	}
	return true
}

func (s *Server) validTunnel(w http.ResponseWriter, r *http.Request, tokenID string) bool {
	secret := bearerToken(r.Header.Get("Authorization"))
	if secret == "" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return false
	}

	matches, err := s.store.TokenMatchesTunnelSecret(tokenID, secret)
	if err != nil {
		http.Error(w, "check tunnel token", http.StatusInternalServerError)
		return false
	}
	if !matches {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return false
	}
	return true
}

func (s *Server) allowWebhook(w http.ResponseWriter, tokenID string) bool {
	if s.webhookLimiter == nil {
		return true
	}

	allowed, retryAfter := s.webhookLimiter.Allow(tokenID)
	if allowed {
		return true
	}
	if retryAfter > 0 {
		seconds := int((retryAfter + time.Second - 1) / time.Second)
		w.Header().Set("Retry-After", strconv.Itoa(seconds))
	}
	http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
	return false
}

func (s *Server) requireAdmin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !sameOriginForUnsafeMethod(r) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		if s.adminPassword == "" {
			if s.allowInsecureAdmin {
				next(w, r)
				return
			}
			http.Error(w, "admin auth not configured", http.StatusServiceUnavailable)
			return
		}
		_, password, ok := r.BasicAuth()
		if !ok || !constantTimeEqual(password, s.adminPassword) {
			w.Header().Set("WWW-Authenticate", `Basic realm="Holloway"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

func bearerToken(header string) string {
	scheme, value, ok := strings.Cut(strings.TrimSpace(header), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return ""
	}
	return strings.TrimSpace(value)
}

func (s *Server) requireAdminHandler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.requireAdmin(next.ServeHTTP)(w, r)
	})
}

func sameOriginForUnsafeMethod(r *http.Request) bool {
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodTrace:
		return true
	}
	if origin := r.Header.Get("Origin"); origin != "" {
		return originMatchesHost(origin, r.Host)
	}
	if referer := r.Header.Get("Referer"); referer != "" {
		return originMatchesHost(referer, r.Host)
	}
	return false
}

func originMatchesHost(rawOrigin, host string) bool {
	parsed, err := url.Parse(rawOrigin)
	if err != nil || parsed.Host == "" {
		return false
	}
	return strings.EqualFold(parsed.Host, host)
}

func constantTimeEqual(got, want string) bool {
	gotHash := sha256.Sum256([]byte(got))
	wantHash := sha256.Sum256([]byte(want))
	return subtle.ConstantTimeCompare(gotHash[:], wantHash[:]) == 1
}
