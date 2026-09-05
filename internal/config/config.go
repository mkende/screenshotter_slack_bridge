// Package config loads and validates the Slack bridge configuration from a
// TOML file, resolving secrets from environment variables where requested.
package config

import (
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

// Image modes for ImageMode, selecting how the screenshot is supplied to Slack.
const (
	// ImageModeUpload fetches the PNG over the (possibly private) network,
	// uploads it to Slack, and makes it public via files.sharedPublicURL so the
	// unfurl can reference it by URL. Works even when the screenshotter server is
	// not reachable from the internet.
	ImageModeUpload = "upload"
	// ImageModePublicURL references the screenshot directly by its public URL,
	// derived from the shared link, without downloading or uploading anything.
	// Requires the screenshotter server to be reachable by Slack.
	ImageModePublicURL = "public_url"
	// ImageModeNone does not unfurl in place at all: the bridge posts the
	// screenshot as a threaded file reply (kept workspace-private, never made
	// public). Requires the bot to be a member of the channel.
	ImageModeNone = "none"
)

// Config holds the bridge's runtime configuration.
type Config struct {
	// ImageMode selects how the screenshot is shown, one of ImageModeUpload
	// (default), ImageModePublicURL, or ImageModeNone. See those constants for the
	// trade-offs. The unfurl modes (upload, public_url) fall back to a threaded
	// file post when the in-place unfurl can't be produced; ImageModeNone always
	// posts the threaded file reply.
	ImageMode string `toml:"image_mode"`

	// ScreenshotterBaseURL is the canonical base URL of the screenshotter server
	// (no trailing slash). It is the single source of truth for the image and the
	// "open on <host>" button; the posted link only supplies the image ID. In
	// ImageModeUpload the bridge fetches the PNG from here (it may be a private
	// address, e.g. "http://screenshotter.internal:8080"). In ImageModePublicURL
	// Slack loads the image from here directly, so it must be reachable by Slack
	// (e.g. "https://screen.corp.example"). Required in both modes.
	ScreenshotterBaseURL string `toml:"screenshotter_base_url"`

	// UnfurlDomains is the list of hostnames that, when seen in a Slack message,
	// the bridge will unfurl. These must match the "App unfurl domains"
	// registered in the Slack app and the hostnames used in your screen/{id}
	// links.
	UnfurlDomains []string `toml:"unfurl_domains"`

	// BotToken is the Slack bot token (xoxb-...). Prefer BotTokenEnvVar.
	BotToken string `toml:"bot_token"`
	// BotTokenEnvVar is the name of an environment variable holding the bot
	// token. When set and non-empty, it takes precedence over BotToken.
	BotTokenEnvVar string `toml:"bot_token_env_var"`

	// AppToken is the Slack app-level token (xapp-...) used for Socket Mode.
	// Prefer AppTokenEnvVar.
	AppToken string `toml:"app_token"`
	// AppTokenEnvVar is the name of an environment variable holding the app
	// token. When set and non-empty, it takes precedence over AppToken.
	AppTokenEnvVar string `toml:"app_token_env_var"`

	// MaxDimension caps the largest side (width or height) of an uploaded
	// image, in pixels. Images larger than this are downscaled before upload,
	// preserving aspect ratio. 0 (the default) disables resizing and uploads
	// the full-resolution PNG.
	MaxDimension int `toml:"max_dimension"`

	// RequestTimeout bounds each outbound request the bridge makes — both
	// fetching an image from the screenshotter server and the Slack API calls
	// (upload, unfurl, user/usergroup lookups). Default: 30s.
	RequestTimeout TOMLDuration `toml:"request_timeout"`

	// MaxConcurrency caps how many link_shared events the bridge handles in
	// parallel; this bounds the network/Slack work ("the rest"). Events beyond
	// this are dropped (logged) rather than queued, so handler goroutines can't
	// pile up unbounded under a flood. Default: 50.
	MaxConcurrency int `toml:"max_concurrency"`

	// MaxImageWorkers caps how many images are decoded/resized in parallel.
	// Image conversion is the memory-heavy step (a decoded image can be far
	// larger than its PNG), so it is bounded separately from, and more tightly
	// than, MaxConcurrency. Default: 4.
	MaxImageWorkers int `toml:"max_image_workers"`

	// MaxLinksPerMessage caps how many screenshot links the bridge renders from
	// a single Slack message, limiting amplification from one post. Default: 4.
	MaxLinksPerMessage int `toml:"max_links_per_message"`

	// BlockExternalUsers, when true, makes the bridge ignore link_shared events
	// triggered by users that are external to the workspace: Slack Connect
	// strangers, users from a different team, and guests (single- and
	// multi-channel). Requires the users:read scope. Default: false.
	BlockExternalUsers bool `toml:"block_external_users"`

	// AllowedUserGroups, when non-empty, restricts rendering to users who belong
	// to at least one of the listed Slack user group IDs (e.g. "S0123ABCD").
	// Requires the usergroups:read scope. Default: empty (no restriction).
	AllowedUserGroups []string `toml:"allowed_usergroups"`

	// ExcludedPaths lists additional first path segments that must never be
	// treated as image IDs, supplementing the built-in set of known server
	// endpoints. Matched case-insensitively. Default: empty.
	ExcludedPaths []string `toml:"excluded_paths"`

	// Debug enables verbose logging of the Socket Mode connection and Slack API
	// traffic. WARNING: debug output can include the bot token and other
	// sensitive data; do not enable it in production. Default: false.
	Debug bool `toml:"debug"`
}

// TOMLDuration is a time.Duration that unmarshals from a TOML string like
// "15s". Mirrors the server's config type for consistency.
type TOMLDuration struct{ time.Duration }

// UnmarshalText parses a Go duration string.
func (d *TOMLDuration) UnmarshalText(b []byte) error {
	dur, err := time.ParseDuration(string(b))
	if err != nil {
		return err
	}
	d.Duration = dur
	return nil
}

// Load reads, decodes, and validates the configuration at path.
func Load(path string) (*Config, error) {
	c := &Config{
		ImageMode:          ImageModeUpload,
		RequestTimeout:     TOMLDuration{30 * time.Second},
		MaxConcurrency:     50,
		MaxImageWorkers:    4,
		MaxLinksPerMessage: 4,
	}

	meta, err := toml.DecodeFile(path, c)
	if err != nil {
		return nil, fmt.Errorf("read config %q: %w", path, err)
	}
	if undecoded := meta.Undecoded(); len(undecoded) > 0 {
		keys := make([]string, len(undecoded))
		for i, k := range undecoded {
			keys[i] = k.String()
		}
		return nil, fmt.Errorf("unknown config keys: %s", strings.Join(keys, ", "))
	}

	c.BotToken, err = resolveSecret("bot_token", c.BotToken, "bot_token_env_var", c.BotTokenEnvVar)
	if err != nil {
		return nil, err
	}
	c.AppToken, err = resolveSecret("app_token", c.AppToken, "app_token_env_var", c.AppTokenEnvVar)
	if err != nil {
		return nil, err
	}

	if err := c.validate(); err != nil {
		return nil, err
	}
	return c, nil
}

func (c *Config) validate() error {
	switch c.ImageMode {
	case ImageModeUpload, ImageModePublicURL, ImageModeNone:
	default:
		return fmt.Errorf("image_mode must be one of %q, %q, %q", ImageModeUpload, ImageModePublicURL, ImageModeNone)
	}

	// base_url is the canonical server URL for both modes (the image source and
	// the button target), so it is always required.
	if c.ScreenshotterBaseURL == "" {
		return fmt.Errorf("screenshotter_base_url is required")
	}
	u, err := url.Parse(c.ScreenshotterBaseURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return fmt.Errorf("screenshotter_base_url must be an absolute URL like \"https://host\"")
	}
	c.ScreenshotterBaseURL = strings.TrimRight(c.ScreenshotterBaseURL, "/")

	if len(c.UnfurlDomains) == 0 {
		return fmt.Errorf("unfurl_domains must list at least one domain")
	}
	for i, d := range c.UnfurlDomains {
		c.UnfurlDomains[i] = strings.ToLower(strings.TrimSpace(d))
		if c.UnfurlDomains[i] == "" {
			return fmt.Errorf("unfurl_domains contains an empty entry")
		}
	}

	if c.BotToken == "" {
		return fmt.Errorf("bot_token (or bot_token_env_var) is required")
	}
	if c.AppToken == "" {
		return fmt.Errorf("app_token (or app_token_env_var) is required")
	}
	if !strings.HasPrefix(c.AppToken, "xapp-") {
		return fmt.Errorf("app_token must be a Socket Mode app-level token starting with \"xapp-\"")
	}
	if c.MaxDimension < 0 {
		return fmt.Errorf("max_dimension must not be negative")
	}
	if c.RequestTimeout.Duration <= 0 {
		return fmt.Errorf("request_timeout must be positive")
	}
	if c.MaxConcurrency < 1 {
		return fmt.Errorf("max_concurrency must be at least 1")
	}
	if c.MaxImageWorkers < 1 {
		return fmt.Errorf("max_image_workers must be at least 1")
	}
	if c.MaxLinksPerMessage < 1 {
		return fmt.Errorf("max_links_per_message must be at least 1")
	}
	for i, g := range c.AllowedUserGroups {
		c.AllowedUserGroups[i] = strings.TrimSpace(g)
		if c.AllowedUserGroups[i] == "" {
			return fmt.Errorf("allowed_usergroups contains an empty entry")
		}
	}
	for i, p := range c.ExcludedPaths {
		c.ExcludedPaths[i] = strings.ToLower(strings.Trim(strings.TrimSpace(p), "/"))
		if c.ExcludedPaths[i] == "" {
			return fmt.Errorf("excluded_paths contains an empty entry")
		}
	}
	return nil
}

// resolveSecret returns the value from the named environment variable when
// envVar is set, otherwise the inline value. It is an error to set both.
func resolveSecret(inlineKey, inlineVal, envKey, envVar string) (string, error) {
	if envVar != "" {
		if inlineVal != "" {
			return "", fmt.Errorf("set only one of %s or %s", inlineKey, envKey)
		}
		v := os.Getenv(envVar)
		if v == "" {
			return "", fmt.Errorf("%s names environment variable %q which is empty or unset", envKey, envVar)
		}
		return v, nil
	}
	return inlineVal, nil
}
