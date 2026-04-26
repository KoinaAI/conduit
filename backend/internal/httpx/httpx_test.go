package httpx

import "testing"

func TestBearerToken(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", ""},
		{"plain", "Bearer abc", "abc"},
		{"lowercase scheme", "bearer abc", "abc"},
		{"mixed case scheme", "BeArEr abc", "abc"},
		{"trims whitespace", "  Bearer   abc  ", "abc"},
		{"non-bearer scheme", "Basic abc", ""},
		{"missing scheme", "abc", ""},
		{"empty token", "Bearer ", ""},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := BearerToken(tc.in); got != tc.want {
				t.Fatalf("BearerToken(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
