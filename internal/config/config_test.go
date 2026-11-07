package config

import (
	"strings"
	"testing"
	"time"
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
	if c.FilesEnabled() || !strings.HasPrefix(c.S3Endpoint, "http://127.0.0.1:") {
		t.Errorf("dev without S3 keys: files enabled %v, endpoint %q", c.FilesEnabled(), c.S3Endpoint)
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
	for _, key := range []string{"RUSCORD_JWT_ED25519_SEED", "RUSCORD_REDIS_PASSWORD", "RUSCORD_LIVEKIT_API_KEY", "RUSCORD_LIVEKIT_API_SECRET",
		"RUSCORD_S3_ACCESS_KEY", "RUSCORD_S3_SECRET_KEY", "RUSCORD_CDN_SECRET"} {
		if !strings.Contains(err.Error(), key) {
			t.Errorf("error does not mention %s: %v", key, err)
		}
	}
}

func TestLoadRejectsShortLiveKitSecret(t *testing.T) {
	_, err := load(env(map[string]string{
		"RUSCORD_ENV":                "staging",
		"RUSCORD_JWT_ED25519_SEED":   strings.Repeat("ab", 32),
		"RUSCORD_REDIS_PASSWORD":     "x",
		"RUSCORD_LIVEKIT_API_KEY":    "key",
		"RUSCORD_LIVEKIT_API_SECRET": "secret",
	}))
	if err == nil || !strings.Contains(err.Error(), "at least 32") {
		t.Fatalf("expected short secret error, got %v", err)
	}
}

func TestLoadRejectsShortCDNSecret(t *testing.T) {
	_, err := load(env(map[string]string{"RUSCORD_ENV": "staging", "RUSCORD_CDN_SECRET": "short"}))
	if err == nil || !strings.Contains(err.Error(), "RUSCORD_CDN_SECRET: must be at least 32") {
		t.Fatalf("expected short CDN secret error, got %v", err)
	}
}

func TestLoadS3KeysComeInPairs(t *testing.T) {
	if _, err := load(env(map[string]string{"RUSCORD_S3_ACCESS_KEY": "k"})); err == nil || !strings.Contains(err.Error(), "both or neither") {
		t.Fatalf("access key without secret: %v", err)
	}
	c, err := load(env(map[string]string{"RUSCORD_S3_ACCESS_KEY": "k", "RUSCORD_S3_SECRET_KEY": "s"}))
	if err != nil || !c.FilesEnabled() {
		t.Fatalf("both keys: %v, enabled %v", err, c.FilesEnabled())
	}
}

func TestLoadTrustedProxies(t *testing.T) {
	c, err := load(env(map[string]string{"RUSCORD_TRUSTED_PROXIES": " 10.0.0.0/8, 172.18.0.3 ,::1"}))
	if err != nil || len(c.TrustedProxies) != 3 || c.TrustedProxies[1].String() != "172.18.0.3/32" || c.TrustedProxies[2].String() != "::1/128" {
		t.Fatalf("parsed %v, %v", c.TrustedProxies, err)
	}
	if _, err := load(env(map[string]string{"RUSCORD_TRUSTED_PROXIES": "nginx"})); err == nil {
		t.Fatal("a host name must be rejected")
	}
	if c, _ := load(env(nil)); len(c.TrustedProxies) != 0 {
		t.Fatal("no proxies are trusted by default")
	}
}

func TestLoadRejectsUnknownEnv(t *testing.T) {
	if _, err := load(env(map[string]string{"RUSCORD_ENV": "production"})); err == nil {
		t.Fatal("expected error for unknown env")
	}
}

func TestLoadJWTSeed(t *testing.T) {
	c, err := load(env(map[string]string{"RUSCORD_JWT_ED25519_SEED": strings.Repeat("0f", 32)}))
	if err != nil || len(c.JWTSeed) != 32 {
		t.Fatalf("seed = %d bytes, err = %v", len(c.JWTSeed), err)
	}
	if _, err := load(env(map[string]string{"RUSCORD_JWT_ED25519_SEED": "abc"})); err == nil {
		t.Fatal("expected error for short seed")
	}
}

func TestAutoMigrateDefaultsToDevOnly(t *testing.T) {
	c, _ := load(env(nil))
	if !c.AutoMigrate {
		t.Error("dev must auto-migrate by default")
	}
	c, _ = load(env(map[string]string{"RUSCORD_ENV": "prod"}))
	if c.AutoMigrate {
		t.Error("prod must not auto-migrate by default")
	}
}

func TestLoadAdminUserIDs(t *testing.T) {
	c, err := load(env(map[string]string{"RUSCORD_ADMIN_USER_IDS": " 019a0000-0000-7000-8000-000000000001, ,019a0000-0000-7000-8000-000000000002"}))
	if err != nil || len(c.AdminUserIDs) != 2 || c.AdminUserIDs[1].String() != "019a0000-0000-7000-8000-000000000002" {
		t.Fatalf("parsed %v, %v", c.AdminUserIDs, err)
	}
	if _, err := load(env(map[string]string{"RUSCORD_ADMIN_USER_IDS": "owner"})); err == nil || !strings.Contains(err.Error(), "RUSCORD_ADMIN_USER_IDS") {
		t.Fatalf("not an id: %v", err)
	}
	if c, _ := load(env(nil)); len(c.AdminUserIDs) != 0 {
		t.Fatalf("default: %v", c.AdminUserIDs)
	}
}

func TestLoadNewcomerWait(t *testing.T) {
	for _, tt := range []struct {
		in   string
		want time.Duration
		bad  bool
	}{
		{in: "", want: 0}, {in: "5", want: 5 * time.Minute}, {in: "-1", bad: true}, {in: "soon", bad: true}, {in: "10081", bad: true},
	} {
		c, err := load(env(map[string]string{"RUSCORD_NEWCOMER_WAIT_MIN": tt.in}))
		if tt.bad != (err != nil) || (!tt.bad && c.NewcomerWait != tt.want) {
			t.Errorf("%q: %v, %v", tt.in, c.NewcomerWait, err)
		}
	}
}
