// herdr-orch is the herdr plugin binary: the long-lived daemon ([[startup]]), the board
// and gate panes ([[panes]]) and the plugin actions ([[actions]]).
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/ellingtonsp/herdr-orch/internal/client"
	"github.com/ellingtonsp/herdr-orch/internal/daemon"
	"github.com/ellingtonsp/herdr-orch/internal/paths"
)

func main() {
	if act := os.Getenv("HERDR_PLUGIN_ACTION_ID"); act != "" {
		action(act)
		return
	}
	mode := "daemon"
	if len(os.Args) > 1 {
		mode = os.Args[1]
	}
	switch mode {
	case "daemon":
		runDaemon()
	case "board":
		runBoard()
	case "gate":
		runGatePopup()
	case "version":
		fmt.Println(daemon.Version)
	case "install-cli":
		dest, err := installCLI()
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Println("horch →", dest)
	default:
		fmt.Fprintf(os.Stderr, "usage: herdr-orch [daemon|board|gate|install-cli|version]\n")
		os.Exit(2)
	}
}

func runDaemon() {
	p, err := paths.Resolve()
	if err != nil {
		log.Fatal(err)
	}
	// Under [[startup]] stdout goes nowhere useful; log to the session's daemon.log.
	if f, err := os.OpenFile(p.Log, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644); err == nil {
		log.SetOutput(f)
	}
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	cfg := daemon.DefaultConfig()
	if d, err := time.ParseDuration(os.Getenv("HORCH_IDLE_REPORT_AFTER")); err == nil {
		cfg.IdleReportAfter = d
	}
	if d, err := time.ParseDuration(os.Getenv("HORCH_BLOCKED_AFTER")); err == nil {
		cfg.BlockedAfter = d
	}
	if d, err := time.ParseDuration(os.Getenv("HORCH_UNOBSERVED_AFTER")); err == nil {
		cfg.UnobservedAfter = d
	}
	if exe, err := os.Executable(); err == nil {
		if r, err := filepath.EvalSymlinks(exe); err == nil {
			exe = r
		}
		if h := filepath.Join(filepath.Dir(exe), "horch"); fileExists(h) {
			cfg.HorchBin = h
		}
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()
	if err := daemon.Run(ctx, daemon.Options{Paths: p, Config: cfg, Logf: log.Printf}); err != nil {
		log.Printf("daemon: %v", err)
		os.Exit(1)
	}
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func herdrBin() string {
	if b := os.Getenv("HERDR_BIN_PATH"); b != "" {
		return b
	}
	return "herdr"
}

func pluginID() string {
	if id := os.Getenv("HERDR_PLUGIN_ID"); id != "" {
		return id
	}
	return paths.PluginID
}

// openPane asks herdr to open one of this plugin's panes.
func openPane(entry, placement string) error {
	args := []string{"plugin", "pane", "open", "--plugin", pluginID(), "--entrypoint", entry, "--placement", placement}
	if placement == "split" {
		args = append(args, "--direction", "right")
		if t := targetPane(); t != "" {
			args = append(args, "--target-pane", t)
		}
	}
	out, err := exec.Command(herdrBin(), args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%v: %s", err, out)
	}
	return nil
}

func action(id string) {
	var err error
	switch id {
	case "board":
		err = openPane("board", "split")
	case "gate":
		err = openPane("gate", "popup")
	case "dispatch-next":
		var c *client.Client
		c, err = client.New()
		if err == nil {
			c.Caller = "human"
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			var ds []any
			err = c.Call(ctx, "dispatch.next", daemon.RunRef{}, &ds)
			if err == nil {
				msg := fmt.Sprintf("dispatched %d task(s)", len(ds))
				_ = exec.Command(herdrBin(), "notification", "show", "horch", "--body", msg).Run()
			}
		}
	case "install-cli":
		var dest string
		dest, err = installCLI()
		if err == nil {
			msg := "horch → " + dest
			if !onPath(filepath.Dir(dest)) {
				msg += " (add " + filepath.Dir(dest) + " to PATH)"
			}
			_ = exec.Command(herdrBin(), "notification", "show", "horch installed", "--body", msg).Run()
			fmt.Println(msg)
		}
	default:
		err = fmt.Errorf("unknown action %q", id)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		_ = exec.Command(herdrBin(), "notification", "show", "horch action failed", "--body", err.Error()).Run()
		os.Exit(1)
	}
}

// targetPane is the pane a split should attach to: the caller's pane, else the focused one.
func targetPane() string {
	if p := os.Getenv("HERDR_PANE_ID"); p != "" {
		return p
	}
	out, err := exec.Command(herdrBin(), "pane", "current").Output()
	if err != nil {
		return ""
	}
	var r struct {
		Result struct {
			Pane struct {
				PaneID string `json:"pane_id"`
			} `json:"pane"`
		} `json:"result"`
	}
	if json.Unmarshal(out, &r) != nil {
		return ""
	}
	return r.Result.Pane.PaneID
}

// installCLI symlinks this plugin's horch into $HORCH_BIN_DIR (default ~/.local/bin). It
// replaces an older symlink but never a regular file it did not create.
func installCLI() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	if r, err := filepath.EvalSymlinks(exe); err == nil {
		exe = r
	}
	src := filepath.Join(filepath.Dir(exe), "horch")
	if !fileExists(src) {
		return "", fmt.Errorf("%s not found; reinstall the plugin", src)
	}
	dir := os.Getenv("HORCH_BIN_DIR")
	if dir == "" {
		home, _ := os.UserHomeDir()
		dir = filepath.Join(home, ".local", "bin")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	dest := filepath.Join(dir, "horch")
	if fi, err := os.Lstat(dest); err == nil {
		if fi.Mode()&os.ModeSymlink == 0 {
			return "", fmt.Errorf("%s exists and is not a symlink; not replacing it", dest)
		}
		if err := os.Remove(dest); err != nil {
			return "", err
		}
	}
	return dest, os.Symlink(src, dest)
}

func onPath(dir string) bool {
	for _, p := range filepath.SplitList(os.Getenv("PATH")) {
		if filepath.Clean(p) == filepath.Clean(dir) {
			return true
		}
	}
	return false
}
