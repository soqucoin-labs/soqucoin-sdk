package loopback

import "testing"

// The rule reads the host as written. Names other than "localhost" are not
// loopback whatever they resolve to, and a spelling of a loopback address
// that is not a valid literal is not one either.
func TestHostReadsTheStringAndResolvesNothing(t *testing.T) {
	for _, tc := range []struct {
		host string
		want bool
	}{
		{"localhost", true},
		{"LOCALHOST", true},
		{"127.0.0.1", true},
		{"127.5.6.7", true},
		{"::1", true},
		{"0:0:0:0:0:0:0:1", true},
		{"127.1", false},
		{"localhost.", false},
		{"host.docker.internal", false},
		{"10.0.0.5", false},
		{"::2", false},
		{"example.invalid", false},
		{"", false},
	} {
		if got := Host(tc.host); got != tc.want {
			t.Errorf("Host(%q) = %v, want %v", tc.host, got, tc.want)
		}
	}
}
