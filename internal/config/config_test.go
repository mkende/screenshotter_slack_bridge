package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadValid(t *testing.T) {
	t.Setenv("SS_TEST_BOT", "xoxb-bot")
	t.Setenv("SS_TEST_APP", "xapp-app")
	p := writeConfig(t, `
screenshotter_base_url = "http://internal:8080/"
unfurl_domains = ["Screen.Corp.Example"]
bot_token_env_var = "SS_TEST_BOT"
app_token_env_var = "SS_TEST_APP"
max_dimension = 1600
request_timeout = "10s"
`)
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.ScreenshotterBaseURL != "http://internal:8080" {
		t.Errorf("trailing slash not trimmed: %q", cfg.ScreenshotterBaseURL)
	}
	if cfg.UnfurlDomains[0] != "screen.corp.example" {
		t.Errorf("domain not lowercased: %q", cfg.UnfurlDomains[0])
	}
	if cfg.BotToken != "xoxb-bot" || cfg.AppToken != "xapp-app" {
		t.Errorf("tokens not resolved from env: %q %q", cfg.BotToken, cfg.AppToken)
	}
	if cfg.RequestTimeout.Duration != 10*time.Second {
		t.Errorf("request_timeout not parsed: %v", cfg.RequestTimeout.Duration)
	}
	if cfg.MaxConcurrency != 50 {
		t.Errorf("default max_concurrency should be 50, got %d", cfg.MaxConcurrency)
	}
	if cfg.MaxImageWorkers != 4 {
		t.Errorf("default max_image_workers should be 4, got %d", cfg.MaxImageWorkers)
	}
	if cfg.MaxLinksPerMessage != 4 {
		t.Errorf("default max_links_per_message should be 4, got %d", cfg.MaxLinksPerMessage)
	}
	if cfg.ImageMode != ImageModeUpload {
		t.Errorf("default image_mode should be %q, got %q", ImageModeUpload, cfg.ImageMode)
	}
}

func TestLoadRejectsUnknownImageMode(t *testing.T) {
	p := writeConfig(t, `
screenshotter_base_url = "http://internal:8080"
unfurl_domains = ["screen.example"]
bot_token = "xoxb-bot"
app_token = "xapp-app"
image_mode = "magic"
`)
	if _, err := Load(p); err == nil {
		t.Error("expected error for unknown image_mode")
	}
}

func TestLoadPublicURLModeRequiresBaseURL(t *testing.T) {
	// base_url is the canonical image source in public_url mode too, so it is
	// required there as well.
	p := writeConfig(t, `
unfurl_domains = ["screen.example"]
bot_token = "xoxb-bot"
app_token = "xapp-app"
image_mode = "public_url"
`)
	if _, err := Load(p); err == nil {
		t.Error("expected error: screenshotter_base_url is required in public_url mode")
	}
}

func TestLoadInlineTokens(t *testing.T) {
	p := writeConfig(t, `
screenshotter_base_url = "http://internal:8080"
unfurl_domains = ["screen.example"]
bot_token = "xoxb-bot"
app_token = "xapp-app"
`)
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.BotToken != "xoxb-bot" {
		t.Errorf("inline bot token not used: %q", cfg.BotToken)
	}
}

func TestLoadRejectsBadAppToken(t *testing.T) {
	p := writeConfig(t, `
screenshotter_base_url = "http://internal:8080"
unfurl_domains = ["screen.example"]
bot_token = "xoxb-bot"
app_token = "not-an-app-token"
`)
	if _, err := Load(p); err == nil {
		t.Error("expected error for app_token without xapp- prefix")
	}
}

func TestLoadRejectsUnknownKey(t *testing.T) {
	p := writeConfig(t, `
screenshotter_base_url = "http://internal:8080"
unfurl_domains = ["screen.example"]
bot_token = "xoxb-bot"
app_token = "xapp-app"
bogus_key = true
`)
	if _, err := Load(p); err == nil {
		t.Error("expected error for unknown config key")
	}
}

func TestResolveSecretRejectsBoth(t *testing.T) {
	t.Setenv("SS_TEST_BOT", "xoxb-bot")
	p := writeConfig(t, `
screenshotter_base_url = "http://internal:8080"
unfurl_domains = ["screen.example"]
bot_token = "xoxb-inline"
bot_token_env_var = "SS_TEST_BOT"
app_token = "xapp-app"
`)
	if _, err := Load(p); err == nil {
		t.Error("expected error when both bot_token and bot_token_env_var are set")
	}
}

func TestLoadRequiresBaseURL(t *testing.T) {
	p := writeConfig(t, `
unfurl_domains = ["screen.example"]
bot_token = "xoxb-bot"
app_token = "xapp-app"
`)
	if _, err := Load(p); err == nil {
		t.Error("expected error when screenshotter_base_url is missing")
	}
}
