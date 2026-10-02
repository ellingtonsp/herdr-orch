package config

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func write(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestMissingFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "nope.toml")
	c, err := LoadFile(p)
	if err != nil || c.Found || c.File != p {
		t.Fatalf("got %+v, %v", c, err)
	}
}

func TestExample(t *testing.T) {
	c, err := LoadFile("../../config.example.toml")
	if err != nil {
		t.Fatal(err)
	}
	if !c.Found || c.Owner.Principal != "stephen" || c.DefaultProject != "novara" {
		t.Fatalf("%+v", c)
	}
	if a := c.Agents["review"]; a.Kind != "codex" || a.Model != "gpt-6.1-sol" || a.Effort != "high" {
		t.Fatalf("review agent %+v", a)
	}
	if c.Agents["build"].Model != "claude-sonnet-5-5" || len(c.Agents) != 5 {
		t.Fatalf("agents %+v", c.Agents)
	}
	if c.Slots != (Slots{Local: 3, IOS: 1, MinFreeRAMPct: 30}) {
		t.Fatalf("slots %+v", c.Slots)
	}
	l := c.Integrations.Linear
	if l.Team != "NVRA" || l.APIKeyEnv != "LINEAR_API_KEY" || l.KeychainService != "horch-linear" {
		t.Fatalf("linear %+v", l)
	}
	p, name, ok := c.Project("")
	if !ok || name != "novara" || p.Repo != "ellingtonsp/novara-mvp" {
		t.Fatalf("project %+v %q %v", p, name, ok)
	}
	if got := p.PlanFile("2026-10-02"); got != "docs/pm/orchestration/2026-10-02.plan.md" {
		t.Fatal(got)
	}
	if got := p.Branch("2026-10-02"); got != "chore/orchestration-2026-10-02" {
		t.Fatal(got)
	}
	if _, _, ok := c.Project("nope"); ok {
		t.Fatal("unexpected project")
	}
}

func TestUnknownKey(t *testing.T) {
	_, err := LoadFile(write(t, "[owner]\nprincipal = \"s\"\nbogus = 1\n"))
	if err == nil || !strings.Contains(err.Error(), "owner.bogus") {
		t.Fatalf("got %v", err)
	}
}

func TestSecretRejected(t *testing.T) {
	for _, body := range []string{
		"[integrations.linear]\napi_key = \"lin_abc123\"\n",
		"[integrations.linear]\nauth_token = \"lin_abc123\"\n",
		"[projects.x]\npassword = \"lin_abc123\"\n",
	} {
		_, err := LoadFile(write(t, body))
		if err == nil || !strings.Contains(err.Error(), "Keychain") {
			t.Fatalf("%q: got %v", body, err)
		}
		if strings.Contains(err.Error(), "lin_abc123") {
			t.Fatalf("error leaks value: %v", err)
		}
	}
	if _, err := LoadFile(write(t, "[integrations.linear]\napi_key_env = \"X\"\nkeychain_service = \"s\"\n")); err != nil {
		t.Fatal(err)
	}
}

func TestInvalidConfigDoesNotExposeValues(t *testing.T) {
	for _, body := range []string{
		"[integrations.linear]\napi_key = lin_sensitive_value\n",
		"[agents.build]\nkind = \"lin_sensitive_value\"\n",
		"default_project = \"lin_sensitive_value\"\n",
		"[owner]\nprincipal = \"lin_sensitive_value\"\nunknown = 1\n",
	} {
		c, err := LoadFile(write(t, body))
		if err == nil {
			t.Fatal("expected refusal")
		}
		if strings.Contains(err.Error(), "lin_sensitive_value") || c.DefaultProject != "" || c.Owner.Principal != "" || c.Found || len(c.Agents) != 0 {
			t.Fatalf("invalid config exposed rejected values: %+v %v", c, err)
		}
	}
}

func TestValidate(t *testing.T) {
	for _, body := range []string{
		"[agents.build]\nkind = \"gpt\"\n",
		"[agents.build]\neffort = \"max\"\n",
		"[slots]\nlocal = -1\n",
		"[slots]\nmin_free_ram_pct = 101\n",
		"default_project = \"ghost\"\n",
	} {
		if _, err := LoadFile(write(t, body)); err == nil {
			t.Fatalf("%q: want error", body)
		}
	}
}

func TestPath(t *testing.T) {
	t.Setenv("HORCH_CONFIG", "/x/y.toml")
	if Path() != "/x/y.toml" {
		t.Fatal(Path())
	}
	t.Setenv("HORCH_CONFIG", "")
	t.Setenv("XDG_CONFIG_HOME", "/xdg")
	if Path() != "/xdg/horch/config.toml" {
		t.Fatal(Path())
	}
	t.Setenv("XDG_CONFIG_HOME", "relative")
	if !strings.HasSuffix(Path(), filepath.Join(".config", "horch", "config.toml")) {
		t.Fatal(Path())
	}
}

func TestLoadUsesHorchConfig(t *testing.T) {
	p := write(t, "[owner]\nprincipal = \"me\"\n")
	t.Setenv("HORCH_CONFIG", p)
	c, err := Load()
	if err != nil || !c.Found || c.File != p || c.Owner.Principal != "me" {
		t.Fatalf("%+v %v", c, err)
	}
}

func TestLinearAPIKey(t *testing.T) {
	old := keychain
	t.Cleanup(func() { keychain = old })
	calls := 0
	keychain = func(service string) (string, error) {
		calls++
		if service != "svc" {
			return "", errors.New("no")
		}
		return "from-keychain", nil
	}
	c := Config{Integrations: Integrations{Linear: Linear{APIKeyEnv: "HORCH_TEST_LIN", KeychainService: "svc"}}}
	t.Setenv("HORCH_TEST_LIN", "from-env")
	if k, err := LinearAPIKey(c); err != nil || k != "from-env" || calls != 0 {
		t.Fatalf("%q %v calls=%d", k, err, calls)
	}
	t.Setenv("HORCH_TEST_LIN", "")
	k, err := LinearAPIKey(c)
	if runtime.GOOS == "darwin" {
		if err != nil || k != "from-keychain" {
			t.Fatalf("%q %v", k, err)
		}
	} else if err == nil {
		t.Fatal("want error off darwin")
	}
	c.Integrations.Linear.KeychainService = ""
	if _, err := LinearAPIKey(c); err == nil || !strings.Contains(err.Error(), "$HORCH_TEST_LIN") {
		t.Fatalf("got %v", err)
	}
	// default env name
	c.Integrations.Linear.APIKeyEnv = ""
	t.Setenv("LINEAR_API_KEY", "dflt")
	if k, _ := LinearAPIKey(c); k != "dflt" {
		t.Fatal(k)
	}
}

func TestMissing(t *testing.T) {
	t.Setenv("LINEAR_API_KEY", "")
	m := Config{File: "/f"}.Missing()
	if len(m) != 5 {
		t.Fatalf("%v", m)
	}
	t.Setenv("LINEAR_API_KEY", "secretvalue")
	c := Config{Found: true, Owner: Owner{Principal: "s"}, Projects: map[string]Project{"a": {}}}
	c.Integrations.Linear.Team = "T"
	if m := c.Missing(); len(m) != 0 {
		t.Fatalf("%v", m)
	}
	c.Integrations.Linear.Team = ""
	for _, s := range c.Missing() {
		if strings.Contains(s, "secretvalue") {
			t.Fatal("leak")
		}
	}
}
