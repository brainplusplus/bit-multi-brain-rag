// Package auth implements API key authentication middleware for the dashboard
// HTTP server, per ADR-0003.
//
// Auth model: global scope (1 valid key = access all projects). Keys are
// configured via DASHBOARD_API_KEYS env var (comma-separated). No per-project
// ACL in MVP.
package auth

import (
	"context"
	"crypto/subtle"
	"net/http"
	"strings"

	"github.com/labstack/echo/v4"
)

// ctxKey is an unexported type to avoid context key collisions.
type ctxKey int

const keyID ctxKey = 0

// Middleware returns an http.Handler that enforces API key auth.
// Valid keys are loaded from the comma-separated allowlist. The request
// is rejected with 401 if the Authorization header is missing or the key
// is not in the allowlist.
//
// Header format: Authorization: Bearer <api-key>
func Middleware(allowlist []string, next http.Handler) http.Handler {
	// Normalize keys once for O(1) lookup + constant-time comparison.
	// Key identity = first 8 chars (for audit log); full key compared
	// with subtle.ConstantTimeCompare to avoid timing attacks.
	normalized := make([]string, 0, len(allowlist))
	for _, k := range allowlist {
		if k = strings.TrimSpace(k); k != "" {
			normalized = append(normalized, k)
		}
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		provided := extractBearer(r)
		if provided == "" {
			reject(w, "missing or malformed Authorization header")
			return
		}

		// Constant-time compare against each allowed key.
		// (Iterating the allowlist is O(n); for small n this is fine.
		//  For large n, a hash map of SHA-256(key) would be better,
		//  but keys should be few in MVP.)
		matchedID := ""
		for _, allowed := range normalized {
			if subtle.ConstantTimeCompare([]byte(provided), []byte(allowed)) == 1 {
				matchedID = keyFingerprint(allowed)
				break
			}
		}
		if matchedID == "" {
			reject(w, "invalid api key")
			return
		}

		// Attach key identity to context for audit logging downstream.
		ctx := context.WithValue(r.Context(), keyID, matchedID)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// KeyIDFromContext returns the fingerprint of the authenticated API key,
// or empty string if unauthenticated. Use for audit logging.
func KeyIDFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(keyID).(string); ok {
		return v
	}
	return ""
}

// extractBearer pulls the token out of "Authorization: Bearer <token>".
func extractBearer(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if h == "" {
		return ""
	}
	const prefix = "Bearer "
	if !strings.HasPrefix(h, prefix) {
		return ""
	}
	return strings.TrimSpace(h[len(prefix):])
}

// keyFingerprint returns a non-reversible short identifier for audit logs.
// First 8 chars of the key — enough to disambiguate keys without leaking
// the full secret into logs.
func keyFingerprint(k string) string {
	if len(k) <= 8 {
		return k
	}
	return k[:8]
}

// reject writes a 401 JSON error response.
func reject(w http.ResponseWriter, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)
	_, _ = w.Write([]byte(`{"error":"unauthorized","message":"` + msg + `"}`))
}

// --- Echo adapter ---
//
// EchoMiddleware adapts the API key auth check to Echo's middleware signature
// (echo.MiddlewareFunc). The dashboard applies it to protected route groups:
//
//	api := e.Group("/api/v1", auth.EchoMiddleware(cfg.DashboardAPIKeys))
//
// The core auth logic lives in Middleware() above (net/http) for testability;
// EchoMiddleware is a thin shim that calls the same validation.

// EchoMiddleware returns an Echo middleware enforcing API key auth.
func EchoMiddleware(allowlist []string) echo.MiddlewareFunc {
	normalized := make([]string, 0, len(allowlist))
	for _, k := range allowlist {
		if k = strings.TrimSpace(k); k != "" {
			normalized = append(normalized, k)
		}
	}
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			provided := extractBearerEcho(c)
			if provided == "" {
				return c.JSON(http.StatusUnauthorized, map[string]string{
					"error":   "unauthorized",
					"message": "missing or malformed Authorization header",
				})
			}
			matchedID := ""
			for _, allowed := range normalized {
				if subtle.ConstantTimeCompare([]byte(provided), []byte(allowed)) == 1 {
					matchedID = keyFingerprint(allowed)
					break
				}
			}
			if matchedID == "" {
				return c.JSON(http.StatusUnauthorized, map[string]string{
					"error":   "unauthorized",
					"message": "invalid api key",
				})
			}
			c.Set("api_key_id", matchedID)
			return next(c)
		}
	}
}

// extractBearerEcho pulls the token out of "Authorization: Bearer <token>"
// from an Echo context.
func extractBearerEcho(c echo.Context) string {
	h := c.Request().Header.Get("Authorization")
	if h == "" {
		return ""
	}
	const prefix = "Bearer "
	if !strings.HasPrefix(h, prefix) {
		return ""
	}
	return strings.TrimSpace(h[len(prefix):])
}
