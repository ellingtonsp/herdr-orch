package paths

import "testing"

func TestSessionPrefersSocket(t *testing.T) {
	t.Setenv("HERDR_SESSION", "stale")
	t.Setenv("HERDR_SOCKET_PATH", "/home/u/.config/herdr/sessions/work/herdr.sock")
	if got := Session(); got != "work" {
		t.Fatalf("got %q", got)
	}
	t.Setenv("HERDR_SOCKET_PATH", "/home/u/.config/herdr/herdr.sock")
	if got := Session(); got != "default" {
		t.Fatalf("got %q", got)
	}
	t.Setenv("HERDR_SOCKET_PATH", "")
	if got := Session(); got != "stale" {
		t.Fatalf("fallback: got %q", got)
	}
}
