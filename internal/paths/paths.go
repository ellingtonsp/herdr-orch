// Package paths resolves where herdr-orch keeps its state for the current herdr session.
//
// HERDR_PLUGIN_STATE_DIR is shared by every herdr session (see docs/herdr-api-notes.md), so
// state is namespaced by session: <state>/sessions/<session>/{orch.db,orch.sock,orch.lock}.
package paths

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const PluginID = "herdr-orch"

type Paths struct {
	Session   string
	Dir       string
	DB        string
	Sock      string
	Lock      string
	Log       string
	Archive   string
	HerdrSock string
}

// Session returns the herdr session name. HERDR_SOCKET_PATH is authoritative
// (".../sessions/<name>/herdr.sock" → <name>, else "default"); HERDR_SESSION is only a
// fallback, because long-lived agent helpers (e.g. codex's app-server) can carry a stale
// HERDR_SESSION from whichever session first launched them.
func Session() string {
	if sock := os.Getenv("HERDR_SOCKET_PATH"); sock != "" {
		return sessionFromSocket(sock)
	}
	if s := os.Getenv("HERDR_SESSION"); s != "" {
		return s
	}
	return "default"
}

func sessionFromSocket(sock string) string {
	if sock == "" {
		return "default"
	}
	dir := filepath.Dir(sock)
	if filepath.Base(filepath.Dir(dir)) == "sessions" {
		return filepath.Base(dir)
	}
	return "default"
}

// stateBase is herdr's plugin state dir for this plugin. The daemon gets it from herdr as
// HERDR_PLUGIN_STATE_DIR; horch, run from agent panes, does not, so it derives the same
// path herdr uses ($XDG_STATE_HOME or ~/.local/state, then herdr/plugins/<id>).
func stateBase() string {
	if d := os.Getenv("HERDR_PLUGIN_STATE_DIR"); d != "" && filepath.Base(d) == PluginID {
		return d
	}
	if d := os.Getenv("HORCH_STATE_DIR"); d != "" {
		return d
	}
	return filepath.Join(xdg("XDG_STATE_HOME", ".local", "state"), "herdr", "plugins", PluginID)
}

// xdg returns $env when it is an absolute path, else ~/<fallback...>.
func xdg(env string, fallback ...string) string {
	if d := os.Getenv(env); filepath.IsAbs(d) {
		return d
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(append([]string{home}, fallback...)...)
}

// Resolve computes the paths for the current environment and creates the state directory.
func Resolve() (Paths, error) {
	sess := Session()
	dir := filepath.Join(stateBase(), "sessions", sess)
	if err := os.MkdirAll(filepath.Join(dir, "archive"), 0o755); err != nil {
		return Paths{}, err
	}
	sock := filepath.Join(dir, "orch.sock")
	// macOS limits unix socket paths to 104 bytes.
	if len(sock) > 100 {
		h := sha256.Sum256([]byte(dir))
		sock = filepath.Join(os.TempDir(), fmt.Sprintf("horch-%d-%s.sock", os.Getuid(), hex.EncodeToString(h[:6])))
	}
	herdrSock := os.Getenv("HERDR_SOCKET_PATH")
	if herdrSock == "" {
		cfg := filepath.Join(xdg("XDG_CONFIG_HOME", ".config"), "herdr")
		if sess == "default" {
			herdrSock = filepath.Join(cfg, "herdr.sock")
		} else {
			herdrSock = filepath.Join(cfg, "sessions", sess, "herdr.sock")
		}
	}
	return Paths{
		Session:   sess,
		Dir:       dir,
		DB:        filepath.Join(dir, "orch.db"),
		Sock:      sock,
		Lock:      filepath.Join(dir, "orch.lock"),
		Log:       filepath.Join(dir, "daemon.log"),
		Archive:   filepath.Join(dir, "archive"),
		HerdrSock: herdrSock,
	}, nil
}

// Safe makes s usable as a file name component.
func Safe(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '/' || r == ':' || r == ' ' {
			return '_'
		}
		return r
	}, s)
}
