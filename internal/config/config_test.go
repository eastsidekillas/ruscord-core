package config

import (
	"strings"
	"testing"
)

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestLoadDefaultsInDev(t *testing.T) {
	c, err := load(env(nil))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !c.IsDev() {
		t.Errorf("Env = %q, want dev", c.Env)
	}
	for _, r := range allRoles {
		if !c.Has(r) {
			t.Errorf("role %s missing from default roles", r)
		}
	}
	if !strings.HasPrefix(c.HTTPAddr, "127.0.0.1:") {
		t.Errorf("dev must listen on loopback by default, got %q", c.HTTPAddr)
	}
}

func TestLoadRoles(t *testing.T) {
	tests := []struct {
		in      string
		want    []Role
		wantErr bool
	}{
		{in: "api", want: []Role{RoleAPI}},
		{in: "gateway, voice", want: []Role{RoleGateway, RoleVoice}},
		{in: "api,api", want: []Role{RoleAPI}},
		{in: "media", wantErr: true},
		{in: "api,", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			c, err := load(env(map[string]string{"RUSCORD_ROLES": tt.in}))
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(c.Roles) != len(tt.want) {
				t.Fatalf("Roles = %v, want %v", c.Roles, tt.want)
			}
			for i := range tt.want {
				if c.Roles[i] != tt.want[i] {
					t.Fatalf("Roles = %v, want %v", c.Roles, tt.want)
				}
			}
		})
	}
}

func TestLoadRequiresSecretsOutsideDev(t *testing.T) {
	_, err := load(env(map[string]string{"RUSCORD_ENV": "prod"}))
	if err == nil {
		t.Fatal("expected error for missing secrets in prod")
	}
	for _, key := range []string{"RUSCORD_REDIS_PASSWORD", "RUSCORD_LIVEKIT_API_KEY", "RUSCORD_LIVEKIT_API_SECRET"} {
		if !strings.Contains(err.Error(), key) {
			t.Errorf("error does not mention %s: %v", key, err)
		}
	}
}

func TestLoadRejectsShortLiveKitSecret(t *testing.T) {
	_, err := load(env(map[string]string{
		"RUSCORD_ENV":                "staging",
		"RUSCORD_REDIS_PASSWORD":     "x",
		"RUSCORD_LIVEKIT_API_KEY":    "key",
		"RUSCORD_LIVEKIT_API_SECRET": "secret",
	}))
	if err == nil || !strings.Contains(err.Error(), "at least 32") {
		t.Fatalf("expected short secret error, got %v", err)
	}
}

func TestLoadRejectsUnknownEnv(t *testing.T) {
	if _, err := load(env(map[string]string{"RUSCORD_ENV": "production"})); err == nil {
		t.Fatal("expected error for unknown env")
	}
}
