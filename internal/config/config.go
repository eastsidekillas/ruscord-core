// Package config loads process configuration from environment variables.
// Every setting has a safe local default except secrets, which must be set
// explicitly outside of the "dev" environment.
package config

import (
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Role is a module of the monolith that this process runs.
type Role string

const (
	RoleAPI     Role = "api"
	RoleGateway Role = "gateway"
	RoleVoice   Role = "voice"
)

var allRoles = []Role{RoleAPI, RoleGateway, RoleVoice}

type Config struct {
	Env             string // dev | staging | prod
	Roles           []Role
	HTTPAddr        string
	ShutdownTimeout time.Duration
	LogLevel        string

	PostgresURL   string
	RedisAddr     string
	RedisPassword string
	NATSURL       string

	// S3 stores avatars and icons (ADR-0019). Without keys in dev, uploads
	// are off (503) and everything else works.
	S3Endpoint  string
	S3Region    string
	S3Bucket    string
	S3AccessKey string
	S3SecretKey string

	LiveKitURL       string
	LiveKitAPIKey    string
	LiveKitAPISecret string

	// JWTSeed is the Ed25519 seed for access tokens. Empty in dev means an
	// ephemeral key: every restart signs everyone out, which is fine locally.
	JWTSeed        []byte
	AllowedOrigins []string
	AutoMigrate    bool
	// MetricsAddr serves GET /metrics on its own listener, never through the
	// public proxy. "off" disables it.
	MetricsAddr string
}

func (c Config) IsDev() bool { return c.Env == "dev" }

// Has reports whether the process runs the given role.
func (c Config) Has(r Role) bool {
	for _, x := range c.Roles {
		if x == r {
			return true
		}
	}
	return false
}

// Load reads the configuration from the environment.
func Load() (Config, error) {
	return load(os.Getenv)
}

func load(getenv func(string) string) (Config, error) {
	get := func(key, def string) string {
		if v := strings.TrimSpace(getenv(key)); v != "" {
			return v
		}
		return def
	}

	c := Config{
		Env:              get("RUSCORD_ENV", "dev"),
		HTTPAddr:         get("RUSCORD_HTTP_ADDR", "127.0.0.1:8080"),
		LogLevel:         get("RUSCORD_LOG_LEVEL", "info"),
		PostgresURL:      get("RUSCORD_POSTGRES_URL", "postgres://ruscord:ruscord@127.0.0.1:5432/ruscord?sslmode=disable"),
		RedisAddr:        get("RUSCORD_REDIS_ADDR", "127.0.0.1:6379"),
		RedisPassword:    get("RUSCORD_REDIS_PASSWORD", ""),
		NATSURL:          get("RUSCORD_NATS_URL", "nats://127.0.0.1:4222"),
		S3Endpoint:       get("RUSCORD_S3_ENDPOINT", "http://127.0.0.1:8333"),
		S3Region:         get("RUSCORD_S3_REGION", "us-east-1"),
		S3Bucket:         get("RUSCORD_S3_BUCKET", "ruscord"),
		S3AccessKey:      get("RUSCORD_S3_ACCESS_KEY", ""),
		S3SecretKey:      get("RUSCORD_S3_SECRET_KEY", ""),
		LiveKitURL:       get("RUSCORD_LIVEKIT_URL", "ws://127.0.0.1:7880"),
		MetricsAddr:      get("RUSCORD_METRICS_ADDR", "127.0.0.1:9090"),
		LiveKitAPIKey:    get("RUSCORD_LIVEKIT_API_KEY", ""),
		LiveKitAPISecret: get("RUSCORD_LIVEKIT_API_SECRET", ""),
	}

	var errs []error

	if seed := get("RUSCORD_JWT_ED25519_SEED", ""); seed != "" {
		b, err := hex.DecodeString(seed)
		if err != nil || len(b) != 32 {
			errs = append(errs, errors.New("RUSCORD_JWT_ED25519_SEED: must be 64 hex characters"))
		}
		c.JWTSeed = b
	}
	for _, o := range strings.Split(get("RUSCORD_ALLOWED_ORIGINS", "http://localhost:4200"), ",") {
		if o = strings.TrimSpace(o); o != "" {
			c.AllowedOrigins = append(c.AllowedOrigins, o)
		}
	}
	c.AutoMigrate = get("RUSCORD_AUTO_MIGRATE", strconv.FormatBool(c.Env == "dev")) == "true"

	switch c.Env {
	case "dev", "staging", "prod":
	default:
		errs = append(errs, fmt.Errorf("RUSCORD_ENV: unknown value %q (want dev, staging or prod)", c.Env))
	}

	roles, err := parseRoles(get("RUSCORD_ROLES", "all"))
	if err != nil {
		errs = append(errs, err)
	}
	c.Roles = roles

	timeout, err := strconv.Atoi(get("RUSCORD_SHUTDOWN_TIMEOUT_SEC", "15"))
	if err != nil || timeout <= 0 {
		errs = append(errs, errors.New("RUSCORD_SHUTDOWN_TIMEOUT_SEC: must be a positive integer"))
	}
	c.ShutdownTimeout = time.Duration(timeout) * time.Second

	if !c.IsDev() {
		for key, val := range map[string]string{
			"RUSCORD_JWT_ED25519_SEED":   string(c.JWTSeed),
			"RUSCORD_REDIS_PASSWORD":     c.RedisPassword,
			"RUSCORD_LIVEKIT_API_KEY":    c.LiveKitAPIKey,
			"RUSCORD_LIVEKIT_API_SECRET": c.LiveKitAPISecret,
			"RUSCORD_S3_ACCESS_KEY":      c.S3AccessKey,
			"RUSCORD_S3_SECRET_KEY":      c.S3SecretKey,
		} {
			if val == "" {
				errs = append(errs, fmt.Errorf("%s: required outside dev", key))
			}
		}
		if len(c.LiveKitAPISecret) > 0 && len(c.LiveKitAPISecret) < 32 {
			errs = append(errs, errors.New("RUSCORD_LIVEKIT_API_SECRET: must be at least 32 characters"))
		}
	}

	if (c.S3AccessKey == "") != (c.S3SecretKey == "") {
		errs = append(errs, errors.New("RUSCORD_S3_ACCESS_KEY and RUSCORD_S3_SECRET_KEY: set both or neither"))
	}

	return c, errors.Join(errs...)
}

// FilesEnabled reports whether S3 credentials are configured.
func (c Config) FilesEnabled() bool { return c.S3AccessKey != "" }

func parseRoles(s string) ([]Role, error) {
	if s == "all" {
		return append([]Role(nil), allRoles...), nil
	}
	var out []Role
	seen := map[Role]bool{}
	for _, part := range strings.Split(s, ",") {
		r := Role(strings.TrimSpace(part))
		valid := false
		for _, known := range allRoles {
			if r == known {
				valid = true
			}
		}
		if !valid {
			return nil, fmt.Errorf("RUSCORD_ROLES: unknown role %q (want all or a list of api, gateway, voice)", r)
		}
		if !seen[r] {
			seen[r] = true
			out = append(out, r)
		}
	}
	return out, nil
}
