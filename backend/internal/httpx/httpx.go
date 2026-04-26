// Package httpx contains small HTTP helpers shared across packages.
package httpx

import "strings"

// BearerToken extracts the bearer credential from a raw Authorization header
// value. It returns the empty string when the value does not begin with the
// Bearer scheme (case-insensitive) or carries no token.
func BearerToken(value string) string {
	scheme, token, ok := strings.Cut(strings.TrimSpace(value), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return ""
	}
	return strings.TrimSpace(token)
}
