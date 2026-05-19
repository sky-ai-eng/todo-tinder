package sandbox

import (
	"encoding/json"
	"strings"
	"testing"

	specs "github.com/opencontainers/runtime-spec/specs-go"
)

// canonicalConfig returns a Config with all required fields set —
// used by spec tests so each test isn't littered with the same
// boilerplate. Real production callers populate these from
// agentproc.RunOptions.
func canonicalConfig() Config {
	return Config{
		RunID:      "abc123def",
		Worktree:   "/data/worktrees/abc123def",
		SDKDir:     "/home/tf/.triagefactory/sdk",
		NodeBinary: "/usr/bin/node",
		Argv:       []string{"/usr/local/bin/node", "/sdk/wrapper.mjs", "-p", "hi"},
		Env: []string{
			"PATH=/usr/local/bin:/usr/bin:/bin",
			"HOME=/work",
		},
	}
}

func TestBuildSpec_RequiredFields(t *testing.T) {
	for _, c := range []struct {
		name   string
		mutate func(*Config)
	}{
		{"empty_runid", func(c *Config) { c.RunID = "" }},
		{"empty_worktree", func(c *Config) { c.Worktree = "" }},
		{"empty_sdkdir", func(c *Config) { c.SDKDir = "" }},
		{"empty_node", func(c *Config) { c.NodeBinary = "" }},
		{"empty_argv", func(c *Config) { c.Argv = nil }},
	} {
		t.Run(c.name, func(t *testing.T) {
			cfg := canonicalConfig()
			c.mutate(&cfg)
			if _, err := buildSpec(cfg, "/var/run/netns/tf-test"); err == nil {
				t.Errorf("buildSpec accepted %s; want validation error", c.name)
			}
		})
	}
	if _, err := buildSpec(canonicalConfig(), ""); err == nil {
		t.Errorf("buildSpec accepted empty netnsPath; want validation error")
	}
}

// TestBuildSpec_PropertyB_NoEnvLeakage is THE load-bearing test for
// SKY-254's security posture. The sandbox MUST NOT propagate any
// credential-shaped env var from the test process into the sandbox's
// process.env. We seed the test process with a sentinel via t.Setenv
// and assert the sentinel appears nowhere in the marshaled spec
// JSON — proves buildSpec doesn't read os.Environ.
//
// If this test ever fails, the credential isolation guarantee is
// broken; do not merge.
func TestBuildSpec_PropertyB_NoEnvLeakage(t *testing.T) {
	// Sentinels in the shape of real credentials the agent might
	// have access to. The marshaled spec must contain NONE.
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-SENTINEL-from-host-env-must-not-leak")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "AWS-SENTINEL-from-host-env-must-not-leak")
	t.Setenv("GITHUB_TOKEN", "ghp_SENTINEL_from_host_env_must_not_leak")
	t.Setenv("ANTHROPIC_FROM_HOST_PROBE", "PROBE-from-host-env-must-not-leak")

	spec, err := buildSpec(canonicalConfig(), "/var/run/netns/tf-test")
	if err != nil {
		t.Fatalf("buildSpec: %v", err)
	}
	data, err := json.Marshal(spec)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	for _, sentinel := range []string{
		"sk-ant-SENTINEL-from-host-env",
		"AWS-SENTINEL-from-host-env",
		"ghp_SENTINEL_from_host_env",
		"PROBE-from-host-env",
	} {
		if strings.Contains(string(data), sentinel) {
			t.Errorf("Property B VIOLATED: spec JSON contains host-env sentinel %q", sentinel)
		}
	}
}

func TestBuildSpec_ProcessEnvVerbatim(t *testing.T) {
	cfg := canonicalConfig()
	cfg.Env = []string{
		"PATH=/usr/local/bin",
		"HOME=/work",
		"CUSTOM_CALLER_VAR=value-from-caller",
	}
	spec, err := buildSpec(cfg, "/var/run/netns/tf-test")
	if err != nil {
		t.Fatalf("buildSpec: %v", err)
	}
	if len(spec.Process.Env) != len(cfg.Env) {
		t.Errorf("spec.Process.Env has %d entries, caller passed %d", len(spec.Process.Env), len(cfg.Env))
	}
	for i, want := range cfg.Env {
		if i >= len(spec.Process.Env) || spec.Process.Env[i] != want {
			t.Errorf("spec.Process.Env[%d] = %q, want %q", i, spec.Process.Env[i], want)
		}
	}
}

func TestBuildSpec_NonRootUID(t *testing.T) {
	spec, err := buildSpec(canonicalConfig(), "/var/run/netns/tf-test")
	if err != nil {
		t.Fatalf("buildSpec: %v", err)
	}
	if spec.Process.User.UID != WorktreeUID {
		t.Errorf("spec.Process.User.UID = %d, want %d (non-root for T3 hardening)",
			spec.Process.User.UID, WorktreeUID)
	}
	if spec.Process.User.UID == 0 {
		t.Errorf("spec.Process.User.UID = 0 (root) — gVisor's user-mode kernel needs in-sandbox UID hardening; running root inside sandbox defeats T3 mitigation")
	}
}

func TestBuildSpec_NoNewPrivileges(t *testing.T) {
	spec, err := buildSpec(canonicalConfig(), "/var/run/netns/tf-test")
	if err != nil {
		t.Fatalf("buildSpec: %v", err)
	}
	if !spec.Process.NoNewPrivileges {
		t.Errorf("spec.Process.NoNewPrivileges = false; required for T3 hardening")
	}
}

func TestBuildSpec_CapabilitiesEmpty(t *testing.T) {
	spec, err := buildSpec(canonicalConfig(), "/var/run/netns/tf-test")
	if err != nil {
		t.Fatalf("buildSpec: %v", err)
	}
	caps := spec.Process.Capabilities
	if caps == nil {
		t.Fatal("spec.Process.Capabilities = nil; want non-nil with empty slices")
	}
	for name, set := range map[string][]string{
		"Bounding":    caps.Bounding,
		"Effective":   caps.Effective,
		"Permitted":   caps.Permitted,
		"Inheritable": caps.Inheritable,
		"Ambient":     caps.Ambient,
	} {
		if len(set) != 0 {
			t.Errorf("%s capabilities not empty: %v", name, set)
		}
	}
}

func TestBuildSpec_NamespacesIncludeNetnsPath(t *testing.T) {
	const want = "/var/run/netns/tf-test"
	spec, err := buildSpec(canonicalConfig(), want)
	if err != nil {
		t.Fatalf("buildSpec: %v", err)
	}
	found := false
	for _, ns := range spec.Linux.Namespaces {
		if ns.Type == specs.NetworkNamespace {
			if ns.Path != want {
				t.Errorf("network namespace Path = %q, want %q", ns.Path, want)
			}
			found = true
		}
	}
	if !found {
		t.Errorf("spec.Linux.Namespaces missing network namespace")
	}
}

func TestBuildSpec_MountsIncludeWorktreeAndSDK(t *testing.T) {
	cfg := canonicalConfig()
	spec, err := buildSpec(cfg, "/var/run/netns/tf-test")
	if err != nil {
		t.Fatalf("buildSpec: %v", err)
	}
	wantMounts := map[string]string{
		"/work":               cfg.Worktree,
		"/sdk":                cfg.SDKDir,
		"/usr/local/bin/node": cfg.NodeBinary,
	}
	for dst, src := range wantMounts {
		var found bool
		for _, m := range spec.Mounts {
			if m.Destination == dst {
				found = true
				if m.Source != src {
					t.Errorf("mount %s: Source = %q, want %q", dst, m.Source, src)
				}
				break
			}
		}
		if !found {
			t.Errorf("spec.Mounts missing destination %q", dst)
		}
	}
}

func TestBuildSpec_ExtraMountsAppended(t *testing.T) {
	cfg := canonicalConfig()
	cfg.ExtraMounts = []Mount{
		{Source: "/run/tf/abc.sock", Destination: "/run/tf.sock", Options: []string{"rw"}},
	}
	spec, err := buildSpec(cfg, "/var/run/netns/tf-test")
	if err != nil {
		t.Fatalf("buildSpec: %v", err)
	}
	found := false
	for _, m := range spec.Mounts {
		if m.Destination == "/run/tf.sock" {
			found = true
			if m.Source != "/run/tf/abc.sock" {
				t.Errorf("extra mount source = %q, want /run/tf/abc.sock", m.Source)
			}
		}
	}
	if !found {
		t.Errorf("extra mount not appended to spec.Mounts")
	}
}

func TestBuildSpec_SeccompSet(t *testing.T) {
	spec, err := buildSpec(canonicalConfig(), "/var/run/netns/tf-test")
	if err != nil {
		t.Fatalf("buildSpec: %v", err)
	}
	if spec.Linux.Seccomp == nil {
		t.Fatal("spec.Linux.Seccomp = nil; want explicit profile")
	}
	if spec.Linux.Seccomp.DefaultAction != specs.ActErrno {
		t.Errorf("Seccomp.DefaultAction = %q, want SCMP_ACT_ERRNO", spec.Linux.Seccomp.DefaultAction)
	}
}
