// Package config loads and stores modon's JSON configuration file and
// resolves default filesystem locations.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Config is the persisted server configuration. Sensitive values (API keys,
// cookies) can also be supplied via environment variables, which always win.
type Config struct {
	PolyPizzaAPIKey string `json:"polypizza_api_key,omitempty"`
	FabCookie       string `json:"fab_cookie,omitempty"`

	// PurchasePolicy controls how paid results are treated:
	//   "ask"  (default) include paid results, flag them for a user decision
	//   "allow"          include paid results without flagging
	//   "deny"           exclude paid results entirely
	PurchasePolicy string  `json:"purchase_policy,omitempty"`
	MaxPrice       float64 `json:"max_price,omitempty"` // 0 = unlimited
	Currency       string  `json:"currency,omitempty"`

	DefaultSources []string `json:"default_sources,omitempty"`

	Preview struct {
		Renderer string `json:"renderer,omitempty"` // auto | blender | f3d | none
		Size     int    `json:"size,omitempty"`
	} `json:"preview,omitempty"`

	// FabAutoClaimFree, when true, lets download_asset claim free Fab listings
	// into the user's library automatically (requires auth cookie).
	FabAutoClaimFree bool `json:"fab_auto_claim_free,omitempty"`

	BlenderPath string `json:"blender_path,omitempty"` // default: look up "blender" in PATH
	F3DPath     string `json:"f3d_path,omitempty"`     // default: look up "f3d" in PATH

	// HTTPTimeoutSeconds bounds remote HTTP calls.
	HTTPTimeoutSeconds int `json:"http_timeout_seconds,omitempty"`
}

// Defaults returns the configuration with all optional fields populated.
func Defaults() *Config {
	c := &Config{
		PurchasePolicy: "ask",
		Currency:       "USD",
		DefaultSources: []string{"local", "polypizza", "fab"},
		BlenderPath:    "blender",
		F3DPath:        "f3d",
	}
	c.Preview.Renderer = "auto"
	c.Preview.Size = 512
	c.HTTPTimeoutSeconds = 30
	return c
}

// ConfigPath resolves the configuration file location.
func ConfigPath() (string, error) {
	if p := os.Getenv("MODON_CONFIG"); p != "" {
		return p, nil
	}
	base, err := os.UserConfigDir() // $XDG_CONFIG_HOME or ~/.config
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "modon", "config.json"), nil
}

// DBPath resolves the SQLite index location.
func DBPath() (string, error) {
	if p := os.Getenv("MODON_DB"); p != "" {
		return p, nil
	}
	return xdgData("modon", "index.db")
}

// CacheDir resolves (and creates) the cache directory for preview images.
func CacheDir() (string, error) {
	if p := os.Getenv("MODON_CACHE"); p != "" {
		return p, os.MkdirAll(p, 0o755)
	}
	base := os.Getenv("XDG_CACHE_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		base = filepath.Join(home, ".cache")
	}
	dir := filepath.Join(base, "modon")
	return dir, os.MkdirAll(dir, 0o755)
}

// DownloadDir is the default landing directory for remote downloads.
func DownloadDir() (string, error) {
	if p := os.Getenv("MODON_DOWNLOADS"); p != "" {
		return p, os.MkdirAll(p, 0o755)
	}
	return xdgData("modon", "downloads")
}

func xdgData(parts ...string) (string, error) {
	base := os.Getenv("XDG_DATA_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		base = filepath.Join(home, ".local", "share")
	}
	p := filepath.Join(append([]string{base}, parts...)...)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return "", err
	}
	return p, nil
}

// Load reads the config file. A missing file yields Defaults.
func Load() (*Config, error) {
	c := Defaults()
	path, err := ConfigPath()
	if err != nil {
		return c, nil
	}
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(raw, c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	c.applyEnv()
	c.normalize()
	return c, nil
}

func (c *Config) applyEnv() {
	if v := os.Getenv("MODON_POLYPIZZA_API_KEY"); v != "" {
		c.PolyPizzaAPIKey = v
	}
	if v := os.Getenv("POLYPIZZA_AUTH_TOKEN"); v != "" { // compatibility with the reference MCP server
		c.PolyPizzaAPIKey = v
	}
	if v := os.Getenv("MODON_FAB_COOKIE"); v != "" {
		c.FabCookie = v
	}
	if v := os.Getenv("MODON_PURCHASE_POLICY"); v != "" {
		c.PurchasePolicy = v
	}
	if v := os.Getenv("MODON_MAX_PRICE"); v != "" {
		var f float64
		if _, err := fmt.Sscanf(v, "%g", &f); err == nil {
			c.MaxPrice = f
		}
	}
}

func (c *Config) normalize() {
	c.PurchasePolicy = strings.ToLower(strings.TrimSpace(c.PurchasePolicy))
	switch c.PurchasePolicy {
	case "ask", "allow", "deny":
	default:
		c.PurchasePolicy = "ask"
	}
	if c.Preview.Size <= 0 {
		c.Preview.Size = 512
	}
	if c.HTTPTimeoutSeconds <= 0 {
		c.HTTPTimeoutSeconds = 30
	}
	if c.Currency == "" {
		c.Currency = "USD"
	}
	if len(c.DefaultSources) == 0 {
		c.DefaultSources = []string{"local", "polypizza", "fab"}
	}
}

// Save persists the config with 0600 permissions (it can hold credentials).
func (c *Config) Save() error {
	c.normalize()
	path, err := ConfigPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, raw, 0o600)
}

// Status summarizes credential/renderer availability for tools and the CLI.
func (c *Config) Status() map[string]any {
	return map[string]any{
		"polypizza_api_key_set": c.PolyPizzaAPIKey != "",
		"fab_cookie_set":        c.FabCookie != "",
		"purchase_policy":       c.PurchasePolicy,
		"max_price":             c.MaxPrice,
		"currency":              c.Currency,
		"default_sources":       c.DefaultSources,
	}
}
