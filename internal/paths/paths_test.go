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

func TestSessionsShareLeaseRegistry(t *testing.T) {
	t.Setenv("HERDR_PLUGIN_STATE_DIR", "")
	t.Setenv("HORCH_STATE_DIR", t.TempDir())
	t.Setenv("HERDR_SOCKET_PATH", "")
	t.Setenv("HERDR_SESSION", "alpha")
	a, err := Resolve()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HERDR_SESSION", "beta")
	b, err := Resolve()
	if err != nil {
		t.Fatal(err)
	}
	if a.DB == b.DB || a.LeaseDB == "" || a.LeaseDB != b.LeaseDB {
		t.Fatalf("session paths: %+v %+v", a, b)
	}
}
