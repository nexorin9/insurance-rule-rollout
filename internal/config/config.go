// Package config loads the six runtime credentials from environment variables
// (see .env.example for key names). It is the single entry point — call sites
// MUST go through Config and MUST NOT call os.Getenv directly so that:
//
//   - missing required keys fail loudly at startup instead of silently
//     producing empty strings later;
//   - validation lives in one place;
//   - tests can inject a clean env via t.Setenv without monkey-patching
//     scattered os.Getenv calls.
//
// Field semantics:
//
//   - InsurancePlatform / InsuranceToken: target for the insurance platform
//     HTTP API that ships new DRG/DIP rule bundles.
//   - HISDSN: DSN for the hospital HIS dictionary table (MySQL / PG / SQLite).
//   - StateDBPath: local SQLite path for Plan / Issue / AuditEvent.
//   - FeishuToken / WeComKey: outbound notification webhook tokens; at least
//     one of them is required (or both, for multi-channel redundancy).
//   - DemoFixtureMode: when true, cmd/demo will skip real HTTP and DB
//     endpoints and use bundled fixtures instead.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// EnvKey* are the canonical environment variable names — duplicated from
// .env.example. Tests should pin against these constants rather than
// re-typing the strings.
const (
	EnvKeyInsurancePlatformBaseURL = "INS_PLATFORM_API_BASE_URL"
	EnvKeyInsurancePlatformToken   = "INS_PLATFORM_API_TOKEN"
	EnvKeyHISDSN                    = "HIS_DB_DSN"
	EnvKeyStateDBPath               = "STATE_DB_PATH"
	EnvKeyFeishuToken               = "FEISHU_BOT_TOKEN"
	EnvKeyWeComKey                  = "WECOM_BOT_KEY"
	EnvKeyDemoFixtureMode           = "DEMO_FIXTURE_MODE"
)

// Config is the immutable credential bundle returned by Load. Call sites
// receive a pointer so they can share one Config across the demo binary.
type Config struct {
	// InsurancePlatform is the BaseURL of the medical-insurance platform
	// that ships monthly DRG/DIP rule bundles. Empty in demo mode.
	InsurancePlatform string

	// InsuranceToken is the bearer token for InsurancePlatform. Empty in
	// demo mode. Not required when DemoFixtureMode is true.
	InsuranceToken string

	// HISDSN is the Go-sql-driver DSN for the HIS dictionary table.
	// Supports MySQL / PG / SQLite forms; the demo binary substitutes
	// ":memory:" via env or DSN directly when running tests.
	HISDSN string

	// StateDBPath is where the Plan / Issue / AuditEvent SQLite file
	// lives. "~/" is expanded against the current user's home dir.
	StateDBPath string

	// FeishuToken is the inbound-webhook token for the Feishu (Lark)
	// notification channel.
	FeishuToken string

	// WeComKey is the inbound-webhook key for the WeCom (企业微信)
	// notification channel.
	WeComKey string

	// DemoFixtureMode toggles fixture-only operation in cmd/demo. When
	// true, InsurancePlatform / InsuranceToken are not required and the
	// demo skips real HTTP calls in favor of bundled testdata/.
	DemoFixtureMode bool
}

// MissingCredentialError is returned when a required credential is absent
// from the environment. Field carries the env-variable name and a human
// hint so operators can self-diagnose from logs.
type MissingCredentialError struct {
	Field       string // env-variable name (e.g. "STATE_DB_PATH")
	Description string // short description of what this credential is for
	Hints       []string
}

func (e *MissingCredentialError) Error() string {
	msg := fmt.Sprintf("missing required credential: %s — %s", e.Field, e.Description)
	if len(e.Hints) > 0 {
		msg += " (请设置 " + e.Field + " 环境变量或参考 .env.example)"
	}
	return msg
}

// Is allows errors.Is to treat any MissingCredentialError the same — useful
// for top-level error classification.
func (e *MissingCredentialError) Is(target error) bool {
	_, ok := target.(*MissingCredentialError)
	return ok
}

// Load reads all six env vars into a Config and validates required fields.
// Returns *MissingCredentialError (typed; recoverable via errors.As) when a
// required field is empty. Validation policy:
//
//   - STATE_DB_PATH is always required.
//   - HIS_DB_DSN is required when DemoFixtureMode is false (demo substitutes
//     SQLite at runtime).
//   - INS_PLATFORM_API_BASE_URL + INS_PLATFORM_API_TOKEN are required when
//     DemoFixtureMode is false.
//   - At least one of FEISHU_BOT_TOKEN / WECOM_BOT_KEY is required when
//     DemoFixtureMode is false.
//
// Empty strings are treated as "not set" — we do NOT silently coerce them.
func Load() (*Config, error) {
	cfg := &Config{
		InsurancePlatform: os.Getenv(EnvKeyInsurancePlatformBaseURL),
		InsuranceToken:    os.Getenv(EnvKeyInsurancePlatformToken),
		HISDSN:            os.Getenv(EnvKeyHISDSN),
		StateDBPath:       os.Getenv(EnvKeyStateDBPath),
		FeishuToken:       os.Getenv(EnvKeyFeishuToken),
		WeComKey:          os.Getenv(EnvKeyWeComKey),
		DemoFixtureMode:   parseBool(os.Getenv(EnvKeyDemoFixtureMode)),
	}

	if cfg.DemoFixtureMode {
		// Demo mode is lenient: STATE_DB_PATH is the only strict field so
		// that the demo can still fail-loud on misconfiguration. When the
		// demo substitutes ":memory:" itself we also accept empty
		// STATE_DB_PATH.
		if strings.TrimSpace(cfg.StateDBPath) == "" {
			// Allow empty in demo mode — cmd/demo will inject ":memory:".
		}
	} else {
		if err := cfg.Validate(); err != nil {
			return nil, err
		}
	}

	if strings.HasPrefix(cfg.StateDBPath, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			cfg.StateDBPath = filepath.Join(home, strings.TrimPrefix(cfg.StateDBPath, "~/"))
		}
	}

	return cfg, nil
}

// Validate enforces the production-mode credential policy described on
// Load. It is exported so cmd/demo (or future binaries) can re-check after
// a config swap.
func (c *Config) Validate() error {
	var missing []*MissingCredentialError

	if strings.TrimSpace(c.StateDBPath) == "" {
		missing = append(missing, &MissingCredentialError{
			Field:       EnvKeyStateDBPath,
			Description: "本地 SQLite 持久化路径（Plan / Issue / AuditEvent）",
			Hints:       []string{"默认 ~/.insurance-rule-rollout/state.db"},
		})
	}

	if c.DemoFixtureMode {
		// In demo mode HIS / insurance / notify are optional.
		return combineMissing(missing)
	}

	if strings.TrimSpace(c.HISDSN) == "" {
		missing = append(missing, &MissingCredentialError{
			Field:       EnvKeyHISDSN,
			Description: "HIS 字典表数据库 DSN（MySQL / PG / SQLite）",
		})
	}

	if strings.TrimSpace(c.InsurancePlatform) == "" {
		missing = append(missing, &MissingCredentialError{
			Field:       EnvKeyInsurancePlatformBaseURL,
			Description: "医保平台下发规则文件 API BaseURL",
		})
	}

	if strings.TrimSpace(c.InsuranceToken) == "" {
		missing = append(missing, &MissingCredentialError{
			Field:       EnvKeyInsurancePlatformToken,
			Description: "医保平台 API 凭据（Authorization 头）",
		})
	}

	if strings.TrimSpace(c.FeishuToken) == "" && strings.TrimSpace(c.WeComKey) == "" {
		missing = append(missing, &MissingCredentialError{
			Field:       EnvKeyFeishuToken + "|" + EnvKeyWeComKey,
			Description: "飞书 / 企微 webhook 凭据（至少填一项）",
		})
	}

	return combineMissing(missing)
}

// HasFeishu reports whether the Feishu channel is configured.
func (c *Config) HasFeishu() bool { return strings.TrimSpace(c.FeishuToken) != "" }

// HasWeCom reports whether the WeCom channel is configured.
func (c *Config) HasWeCom() bool { return strings.TrimSpace(c.WeComKey) != "" }

// HasInsurancePlatform reports whether both base URL and token are set
// (production mode requires both together).
func (c *Config) HasInsurancePlatform() bool {
	return strings.TrimSpace(c.InsurancePlatform) != "" && strings.TrimSpace(c.InsuranceToken) != ""
}

// HasHIS reports whether the HIS DSN is configured.
func (c *Config) HasHIS() bool { return strings.TrimSpace(c.HISDSN) != "" }

// parseBool accepts "1", "true", "TRUE", "yes", "on" as true and everything
// else as false — same convention as encoding.TextMarshaler. Empty string
// is treated as false so DEMO_FIXTURE_MODE= (unset) is the safe default.
func parseBool(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

// combineMissing joins missing-credential errors into a single error.
// Returns nil when the slice is empty so callers can short-circuit.
func combineMissing(errs []*MissingCredentialError) error {
	if len(errs) == 0 {
		return nil
	}
	if len(errs) == 1 {
		return errs[0]
	}
	msgs := make([]string, 0, len(errs))
	for _, e := range errs {
		msgs = append(msgs, e.Error())
	}
	return errors.New("multiple credentials missing: " + strings.Join(msgs, " | "))
}