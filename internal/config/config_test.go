package config

import (
	"errors"
	"strings"
	"testing"
)

// clearAllEnv removes every credential env var the package reads so each
// test starts from a known-empty baseline. t.Setenv cannot clear, only set;
// we therefore save/restore the prior values around each call.
func clearAllEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		EnvKeyInsurancePlatformBaseURL,
		EnvKeyInsurancePlatformToken,
		EnvKeyHISDSN,
		EnvKeyStateDBPath,
		EnvKeyFeishuToken,
		EnvKeyWeComKey,
		EnvKeyDemoFixtureMode,
	} {
		t.Setenv(k, "")
	}
}

// setAllRequired sets STATE_DB_PATH plus the demo flag — enough for
// demo-mode tests. Production-mode tests layer on the rest.
func setAllRequired(t *testing.T, demoMode bool) {
	t.Helper()
	t.Setenv(EnvKeyStateDBPath, "~/.insurance-rule-rollout/state.db")
	if demoMode {
		t.Setenv(EnvKeyDemoFixtureMode, "true")
	} else {
		t.Setenv(EnvKeyDemoFixtureMode, "false")
	}
}

func TestLoad_DemoMode_EmptyEnvSucceeds(t *testing.T) {
	clearAllEnv(t)
	t.Setenv(EnvKeyDemoFixtureMode, "true")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("demo mode with empty env should load, got %v", err)
	}
	if !cfg.DemoFixtureMode {
		t.Fatalf("expected DemoFixtureMode=true")
	}
	if cfg.StateDBPath != "" {
		// OK either way — empty is allowed in demo mode
	}
	if cfg.InsurancePlatform != "" {
		t.Fatalf("expected empty InsurancePlatform, got %q", cfg.InsurancePlatform)
	}
}

func TestLoad_DemoMode_HomeDirExpansion(t *testing.T) {
	clearAllEnv(t)
	t.Setenv(EnvKeyDemoFixtureMode, "true")
	t.Setenv(EnvKeyStateDBPath, "~/state.db")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("demo mode with ~/path should load, got %v", err)
	}
	if strings.HasPrefix(cfg.StateDBPath, "~/") {
		t.Fatalf("expected ~/ to be expanded, still have %q", cfg.StateDBPath)
	}
	if !strings.HasSuffix(cfg.StateDBPath, "/state.db") {
		t.Fatalf("expected expanded path to end with /state.db, got %q", cfg.StateDBPath)
	}
}

func TestLoad_ProductionMode_EmptyEnvFails(t *testing.T) {
	clearAllEnv(t)
	t.Setenv(EnvKeyDemoFixtureMode, "false")

	_, err := Load()
	if err == nil {
		t.Fatalf("production mode with empty env should fail")
	}
	var mce *MissingCredentialError
	if !errors.As(err, &mce) {
		// We accept either a single *MissingCredentialError or a
		// combined errors.New — verify at least the env-variable name
		// appears in the message.
		if !strings.Contains(err.Error(), EnvKeyStateDBPath) {
			t.Fatalf("expected error to mention %s, got %v", EnvKeyStateDBPath, err)
		}
		return
	}
	if mce.Field != EnvKeyStateDBPath {
		t.Fatalf("expected first missing field to be %s, got %q", EnvKeyStateDBPath, mce.Field)
	}
	if !strings.Contains(mce.Error(), EnvKeyStateDBPath) {
		t.Fatalf("error message should mention env var name: %v", mce)
	}
}

func TestLoad_ProductionMode_MissingAllFields(t *testing.T) {
	clearAllEnv(t)
	t.Setenv(EnvKeyDemoFixtureMode, "false")

	_, err := Load()
	if err == nil {
		t.Fatalf("expected error when no creds set in production mode")
	}
	// Should report at least STATE_DB_PATH; combined error reports all.
	if !strings.Contains(err.Error(), EnvKeyStateDBPath) {
		t.Fatalf("error should mention %s, got %v", EnvKeyStateDBPath, err)
	}
	if !strings.Contains(err.Error(), EnvKeyHISDSN) {
		t.Fatalf("error should mention %s, got %v", EnvKeyHISDSN, err)
	}
	if !strings.Contains(err.Error(), EnvKeyInsurancePlatformBaseURL) {
		t.Fatalf("error should mention %s, got %v", EnvKeyInsurancePlatformBaseURL, err)
	}
}

func TestLoad_ProductionMode_OnlyFeishu_Succeeds(t *testing.T) {
	clearAllEnv(t)
	setAllRequired(t, false)
	t.Setenv(EnvKeyHISDSN, "user:pwd@tcp(host:3306)/his_db")
	t.Setenv(EnvKeyInsurancePlatformBaseURL, "https://ins.example.com")
	t.Setenv(EnvKeyInsurancePlatformToken, "secret-token")
	t.Setenv(EnvKeyFeishuToken, "feishu-bot-token")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("expected success with Feishu channel set, got %v", err)
	}
	if !cfg.HasFeishu() {
		t.Fatalf("expected HasFeishu=true")
	}
	if cfg.HasWeCom() {
		t.Fatalf("expected HasWeCom=false")
	}
	if !cfg.HasInsurancePlatform() {
		t.Fatalf("expected HasInsurancePlatform=true")
	}
	if !cfg.HasHIS() {
		t.Fatalf("expected HasHIS=true")
	}
}

func TestLoad_ProductionMode_OnlyWeCom_Succeeds(t *testing.T) {
	clearAllEnv(t)
	setAllRequired(t, false)
	t.Setenv(EnvKeyHISDSN, "user:pwd@tcp(host:3306)/his_db")
	t.Setenv(EnvKeyInsurancePlatformBaseURL, "https://ins.example.com")
	t.Setenv(EnvKeyInsurancePlatformToken, "secret-token")
	t.Setenv(EnvKeyWeComKey, "wecom-bot-key")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("expected success with WeCom channel set, got %v", err)
	}
	if cfg.HasFeishu() {
		t.Fatalf("expected HasFeishu=false")
	}
	if !cfg.HasWeCom() {
		t.Fatalf("expected HasWeCom=true")
	}
}

func TestLoad_ProductionMode_BothChannels_Succeeds(t *testing.T) {
	clearAllEnv(t)
	setAllRequired(t, false)
	t.Setenv(EnvKeyHISDSN, "postgres://u:p@host:5432/his")
	t.Setenv(EnvKeyInsurancePlatformBaseURL, "https://ins.example.com")
	t.Setenv(EnvKeyInsurancePlatformToken, "tok")
	t.Setenv(EnvKeyFeishuToken, "feishu")
	t.Setenv(EnvKeyWeComKey, "wecom")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("expected success with both channels, got %v", err)
	}
	if !cfg.HasFeishu() || !cfg.HasWeCom() {
		t.Fatalf("expected both channels active")
	}
}

func TestLoad_ProductionMode_NoNotifyChannel_Fails(t *testing.T) {
	clearAllEnv(t)
	setAllRequired(t, false)
	t.Setenv(EnvKeyHISDSN, "user:pwd@tcp(host:3306)/his_db")
	t.Setenv(EnvKeyInsurancePlatformBaseURL, "https://ins.example.com")
	t.Setenv(EnvKeyInsurancePlatformToken, "secret-token")
	// FEISHU_BOT_TOKEN / WECOM_BOT_KEY both empty

	_, err := Load()
	if err == nil {
		t.Fatalf("expected failure when both notify channels missing")
	}
	if !strings.Contains(err.Error(), EnvKeyFeishuToken) {
		t.Fatalf("error should mention notify channels, got %v", err)
	}
}

func TestLoad_ProductionMode_HISMissing_Fails(t *testing.T) {
	clearAllEnv(t)
	setAllRequired(t, false)
	t.Setenv(EnvKeyInsurancePlatformBaseURL, "https://ins.example.com")
	t.Setenv(EnvKeyInsurancePlatformToken, "tok")
	t.Setenv(EnvKeyFeishuToken, "feishu")
	// HIS_DSN empty

	_, err := Load()
	if err == nil {
		t.Fatalf("expected failure when HIS_DSN missing")
	}
	if !strings.Contains(err.Error(), EnvKeyHISDSN) {
		t.Fatalf("error should mention HIS_DSN, got %v", err)
	}
}

func TestLoad_ProductionMode_InsurancePlatformMissing_Fails(t *testing.T) {
	clearAllEnv(t)
	setAllRequired(t, false)
	t.Setenv(EnvKeyHISDSN, "user:pwd@tcp(host:3306)/his_db")
	t.Setenv(EnvKeyFeishuToken, "feishu")
	// InsurancePlatform URL/token missing

	_, err := Load()
	if err == nil {
		t.Fatalf("expected failure when insurance creds missing")
	}
	if !strings.Contains(err.Error(), EnvKeyInsurancePlatformBaseURL) {
		t.Fatalf("error should mention %s, got %v", EnvKeyInsurancePlatformBaseURL, err)
	}
	if !strings.Contains(err.Error(), EnvKeyInsurancePlatformToken) {
		t.Fatalf("error should mention %s, got %v", EnvKeyInsurancePlatformToken, err)
	}
}

func TestValidate_CanBeCalledDirectly(t *testing.T) {
	// Set every required field except HIS_DSN so Validate returns a
	// single typed MissingCredentialError (not the combined wrapper).
	cfg := &Config{
		StateDBPath:         "/tmp/state.db",
		InsurancePlatform:   "https://ins.example.com",
		InsuranceToken:      "tok",
		FeishuToken:         "feishu",
		DemoFixtureMode:     false,
		// HISDSN deliberately empty
	}
	err := cfg.Validate()
	if err == nil {
		t.Fatalf("Config missing HIS_DSN should fail Validate")
	}
	var mce *MissingCredentialError
	if !errors.As(err, &mce) {
		t.Fatalf("expected *MissingCredentialError, got %T: %v", err, err)
	}
	if mce.Field != EnvKeyHISDSN {
		t.Fatalf("expected missing field %s, got %q", EnvKeyHISDSN, mce.Field)
	}

	// Demo mode skips strict validation but Validate() called directly
	// is still strict (production semantics). For demo leniency use Load.
}

func TestValidate_AllEmptyProducesCombinedError(t *testing.T) {
	cfg := &Config{} // all empty, production mode
	err := cfg.Validate()
	if err == nil {
		t.Fatalf("empty Config in production mode should fail Validate")
	}
	// 5 missing → combined errors.New with all names listed.
	for _, key := range []string{
		EnvKeyStateDBPath,
		EnvKeyHISDSN,
		EnvKeyInsurancePlatformBaseURL,
		EnvKeyInsurancePlatformToken,
		EnvKeyFeishuToken,
	} {
		if !strings.Contains(err.Error(), key) {
			t.Errorf("combined error should mention %s, got %v", key, err)
		}
	}
}

func TestValidate_DemoMode_SkipsHISAndInsuranceAndNotify(t *testing.T) {
	cfg := &Config{
		StateDBPath:     "/tmp/state.db",
		DemoFixtureMode: true,
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("demo-mode Validate should only check STATE_DB_PATH, got %v", err)
	}
}

func TestParseBool(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"", false},
		{"0", false},
		{"1", true},
		{"true", true},
		{"TRUE", true},
		{"True", true},
		{"yes", true},
		{"YES", true},
		{"on", true},
		{"on ", true},
		{"  on  ", true},
		{"false", false},
		{"no", false},
		{"off", false},
		{"random", false},
	}
	for _, tc := range cases {
		if got := parseBool(tc.in); got != tc.want {
			t.Errorf("parseBool(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestLoad_DemoMode_DemoFlagVariants(t *testing.T) {
	for _, v := range []string{"1", "true", "TRUE", "yes", "on"} {
		clearAllEnv(t)
		t.Setenv(EnvKeyDemoFixtureMode, v)
		cfg, err := Load()
		if err != nil {
			t.Fatalf("expected load to succeed with DEMO_FIXTURE_MODE=%q, got %v", v, err)
		}
		if !cfg.DemoFixtureMode {
			t.Errorf("expected DemoFixtureMode=true for input %q", v)
		}
	}
}

func TestMissingCredentialError_Is(t *testing.T) {
	a := &MissingCredentialError{Field: "X"}
	var b error = &MissingCredentialError{Field: "Y"}
	if !errors.Is(a, b) {
		t.Fatalf("expected errors.Is to match two MissingCredentialError values")
	}
	if errors.Is(a, errors.New("other")) {
		t.Fatalf("expected errors.Is to NOT match an unrelated error")
	}
}

func TestLoad_WhitespaceTreatedAsEmpty(t *testing.T) {
	clearAllEnv(t)
	t.Setenv(EnvKeyDemoFixtureMode, "false")
	t.Setenv(EnvKeyStateDBPath, "   ")
	t.Setenv(EnvKeyHISDSN, "\t")

	_, err := Load()
	if err == nil {
		t.Fatalf("whitespace-only values should be treated as missing")
	}
	if !strings.Contains(err.Error(), EnvKeyStateDBPath) {
		t.Fatalf("expected error to mention %s, got %v", EnvKeyStateDBPath, err)
	}
}