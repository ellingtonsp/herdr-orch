// Package config loads the per-user horch config (~/.config/horch/config.toml).
//
// The file holds preferences only: who the owner is, default agents, slot limits, projects,
// integration settings. Secrets never live here; they come from the environment or the macOS
// Keychain, and a secret-looking key in the file is rejected without echoing its value.
package config

import (
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"
)

const defaultLinearKeyEnv = "LINEAR_API_KEY"

// Config is the parsed config file. File and Found describe where it came from.
type Config struct {
	File           string             `toml:"-" json:"file"`
	Found          bool               `toml:"-" json:"found"`
	Web            Web                `toml:"web" json:"web"`
	Owner          Owner              `toml:"owner" json:"owner"`
	DefaultProject string             `toml:"default_project" json:"default_project"`
	Agents         map[string]Agent   `toml:"agents" json:"agents"`
	Slots          Slots              `toml:"slots" json:"slots"`
	Projects       map[string]Project `toml:"projects" json:"projects"`
	Integrations   Integrations       `toml:"integrations" json:"integrations"`
}

// Web holds listener preferences; identity is supplied by a trusted tailnet proxy.
type Web struct {
	Listen           string `toml:"listen" json:"listen"`
	OwnerPrincipal   string `toml:"owner_principal" json:"owner_principal"`
	IdentityHeader   string `toml:"identity_header" json:"identity_header"`
	AllowLocalWrites bool   `toml:"allow_local_writes" json:"allow_local_writes"`
}

const DefaultWebListen = "127.0.0.1:7171"

func (c Config) WebSettings() Web {
	w := c.Web
	if w.Listen == "" {
		w.Listen = DefaultWebListen
	}
	if w.OwnerPrincipal == "" {
		w.OwnerPrincipal = c.Owner.Principal
	}
	return w
}
func ValidateWebListen(address string) error {
	_, port, err := net.SplitHostPort(address)
	if err != nil {
		return errors.New("web.listen: want host:port")
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 0 || n > 65535 {
		return errors.New("web.listen: invalid port")
	}
	return nil
}

// Owner is the human running horch. Principal is the identity whose plan edits count as approvals.
type Owner struct {
	Principal string `toml:"principal" json:"principal"`
	Name      string `toml:"name" json:"name"`
	Email     string `toml:"email" json:"email"` // git author for exports
}

// Agent is a default agent for a role (build, review, security_review, ux_review, orchestrator).
type Agent struct {
	Kind   string `toml:"kind" json:"kind"` // claude | codex | pi
	Model  string `toml:"model" json:"model"`
	Effort string `toml:"effort" json:"effort"` // codex reasoning effort: low | medium | high | xhigh
}

// Slots caps concurrent work and sets the RAM floor for starting more.
type Slots struct {
	Local         int `toml:"local" json:"local"`
	IOS           int `toml:"ios" json:"ios"`
	MinFreeRAMPct int `toml:"min_free_ram_pct" json:"min_free_ram_pct"`
}

// Project describes one repo horch plans for. "<date>" in PlanPath/DayLogBranch is the plan day.
type Project struct {
	Repo         string `toml:"repo" json:"repo"` // owner/name
	PlanPath     string `toml:"plan_path" json:"plan_path"`
	DayLogBranch string `toml:"day_log_branch" json:"day_log_branch"`
}

// Integrations groups third-party settings.
type Integrations struct {
	Linear Linear `toml:"linear" json:"linear"`
}

// Linear names where the API key comes from; it never holds the key itself.
type Linear struct {
	Team            string `toml:"team" json:"team"`
	APIKeyEnv       string `toml:"api_key_env" json:"api_key_env"`           // default LINEAR_API_KEY
	KeychainService string `toml:"keychain_service" json:"keychain_service"` // macOS generic-password service
}

// PlanFile is the plan path for day (YYYY-MM-DD).
func (p Project) PlanFile(day string) string {
	return strings.ReplaceAll(p.PlanPath, "<date>", day)
}

// Branch is the day-log branch for day (YYYY-MM-DD).
func (p Project) Branch(day string) string {
	return strings.ReplaceAll(p.DayLogBranch, "<date>", day)
}

// Path returns the config file location: $HORCH_CONFIG, else $XDG_CONFIG_HOME/horch/config.toml
// when XDG_CONFIG_HOME is absolute, else ~/.config/horch/config.toml.
func Path() string {
	if p := os.Getenv("HORCH_CONFIG"); p != "" {
		return p
	}
	if d := os.Getenv("XDG_CONFIG_HOME"); filepath.IsAbs(d) {
		return filepath.Join(d, "horch", "config.toml")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "horch", "config.toml")
}

// Load reads Path().
func Load() (Config, error) { return LoadFile(Path()) }

// LoadFile reads and validates the config at path. A missing file yields the zero Config
// with Found=false and no error.
func LoadFile(path string) (Config, error) {
	c := Config{File: path}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return c, err
	}
	var raw map[string]any
	if _, err := toml.Decode(string(data), &raw); err != nil {
		return c, configDecodeError(path, err)
	}
	// Before the unknown-key check so the message is the helpful one. Never includes values.
	if keys := secretKeys(raw, nil); len(keys) > 0 {
		return c, fmt.Errorf("%s: %s looks like a secret; secrets belong in the environment or macOS Keychain, never in this file (name the env var with api_key_env or the Keychain service with keychain_service)", path, strings.Join(keys, ", "))
	}
	md, err := toml.Decode(string(data), &c)
	if err != nil {
		return Config{File: path}, configDecodeError(path, err)
	}
	if un := md.Undecoded(); len(un) > 0 {
		names := make([]string, len(un))
		for i, k := range un {
			names[i] = k.String()
		}
		return Config{File: path}, fmt.Errorf("%s: unknown keys: %s", path, strings.Join(names, ", "))
	}
	c.File, c.Found = path, true
	if err := c.Validate(); err != nil {
		return Config{File: path}, fmt.Errorf("%s: %w", path, err)
	}
	return c, nil
}

// Decoder diagnostics can contain rejected input. Keep only the location, never values.
func configDecodeError(path string, err error) error {
	var pe toml.ParseError
	if errors.As(err, &pe) {
		return fmt.Errorf("%s: invalid TOML at line %d", path, pe.Position.Line)
	}
	return fmt.Errorf("%s: invalid config field type", path)
}

// secretKeys returns dotted names of keys that look like secrets, sorted.
func secretKeys(m map[string]any, prefix []string) []string {
	var out []string
	for k, v := range m {
		path := append(append([]string{}, prefix...), k)
		if secretName(k) {
			out = append(out, strings.Join(path, "."))
		}
		switch t := v.(type) {
		case map[string]any:
			out = append(out, secretKeys(t, path)...)
		case []map[string]any:
			for _, e := range t {
				out = append(out, secretKeys(e, path)...)
			}
		}
	}
	sort.Strings(out)
	return out
}

func secretName(k string) bool {
	k = strings.ToLower(k)
	if strings.HasSuffix(k, "_env") || strings.HasSuffix(k, "_keychain") {
		return false
	}
	switch k {
	case "api_key", "apikey", "key":
		return true
	}
	for _, w := range []string{"token", "secret", "password"} {
		if strings.Contains(k, w) {
			return true
		}
	}
	return false
}

var (
	agentKinds = map[string]bool{"claude": true, "codex": true, "pi": true}
	efforts    = map[string]bool{"low": true, "medium": true, "high": true, "xhigh": true}
)

// Validate checks enum values, ranges, and that DefaultProject exists.
func (c Config) Validate() error {
	for _, ch := range c.Web.IdentityHeader {
		if !((ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') || (ch >= '0' && ch <= '9') || strings.ContainsRune("!#$%&'*+-.^_`|~", ch)) {
			return errors.New("web.identity_header: want an HTTP header name")
		}
	}
	if err := ValidateWebListen(c.WebSettings().Listen); err != nil {
		return err
	}
	roles := make([]string, 0, len(c.Agents))
	for r := range c.Agents {
		roles = append(roles, r)
	}
	sort.Strings(roles)
	for _, r := range roles {
		a := c.Agents[r]
		if a.Kind != "" && !agentKinds[a.Kind] {
			return fmt.Errorf("agents.%s.kind: want claude, codex or pi", r)
		}
		if a.Effort != "" && !efforts[a.Effort] {
			return fmt.Errorf("agents.%s.effort: want low, medium, high or xhigh", r)
		}
	}
	if c.Slots.Local < 0 || c.Slots.IOS < 0 {
		return errors.New("slots must be non-negative")
	}
	if c.Slots.MinFreeRAMPct < 0 || c.Slots.MinFreeRAMPct > 100 {
		return fmt.Errorf("slots.min_free_ram_pct %d: want 0..100", c.Slots.MinFreeRAMPct)
	}
	if c.DefaultProject != "" {
		if _, ok := c.Projects[c.DefaultProject]; !ok {
			return errors.New("default_project is not defined under [projects]")
		}
	}
	return nil
}

// Project looks up a project; name "" means DefaultProject. It returns the resolved name.
func (c Config) Project(name string) (Project, string, bool) {
	if name == "" {
		name = c.DefaultProject
	}
	p, ok := c.Projects[name]
	return p, name, ok
}

// keychain reads a macOS generic-password item. A var so tests can stub it.
var keychain = func(service string) (string, error) {
	out, err := exec.Command("security", "find-generic-password", "-s", service, "-w").Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// LinearAPIKey resolves the Linear key from the environment, then the macOS Keychain.
// The error names only where it looked, never a value.
func LinearAPIKey(c Config) (string, error) {
	l := c.Integrations.Linear
	env := l.APIKeyEnv
	if env == "" {
		env = defaultLinearKeyEnv
	}
	if v := os.Getenv(env); v != "" {
		return v, nil
	}
	where := "$" + env
	if l.KeychainService != "" && runtime.GOOS == "darwin" {
		if v, err := keychain(l.KeychainService); err == nil && v != "" {
			return v, nil
		}
		where += " or Keychain service " + l.KeychainService
	}
	return "", fmt.Errorf("linear api key not found in %s", where)
}

// Missing lists what a first-run settings screen should ask for.
func (c Config) Missing() []string {
	var m []string
	if !c.Found {
		m = append(m, "config file ("+c.File+")")
	}
	if c.Owner.Principal == "" {
		m = append(m, "owner.principal")
	}
	if len(c.Projects) == 0 {
		m = append(m, "projects")
	}
	if c.Integrations.Linear.Team == "" {
		m = append(m, "integrations.linear.team")
	}
	if _, err := LinearAPIKey(c); err != nil {
		m = append(m, "linear api key (env or Keychain)")
	}
	return m
}
