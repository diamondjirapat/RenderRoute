package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Rule maps a process executable name or desktop app ID to a preferred GPU selector.
type Rule struct {
	Target         string            `json:"target"`                     // Process name or app identifier (canonical lowercase key)
	DisplayName    string            `json:"display_name,omitempty"`     // Friendly display label
	GPU            string            `json:"gpu"`                        // GPU selector ("dgpu", "igpu", PCI slot, etc.)
	DesktopID      string            `json:"desktop_id,omitempty"`       // Optional associated .desktop file ID
	DesktopPatched bool              `json:"desktop_patched,omitempty"`  // Whether ~/.local/share/applications override was applied
	ExtraEnv       map[string]string `json:"extra_env,omitempty"`        // Optional user env overrides
}

// Config is the persisted user configuration at ~/.config/renderroute/config.json.
type Config struct {
	DefaultGPU string          `json:"default_gpu"`
	Rules      map[string]Rule `json:"rules"`
}

// ConfigPath returns the XDG-compliant path to renderroute's config file.
func ConfigPath() (string, error) {
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		return filepath.Join(xdg, "renderroute", "config.json"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "renderroute", "config.json"), nil
}

// Default returns a fresh default Config.
func Default() *Config {
	return &Config{
		DefaultGPU: "igpu",
		Rules:      map[string]Rule{},
	}
}

// Load reads config from the default XDG path.
func Load() (*Config, error) {
	p, err := ConfigPath()
	if err != nil {
		return Default(), err
	}
	return LoadFrom(p)
}

// LoadFrom reads config from a specific file path, returning defaults if it doesn't exist.
func LoadFrom(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Default(), nil
		}
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}
	cfg := Default()
	if err := json.Unmarshal(b, cfg); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}
	if cfg.Rules == nil {
		cfg.Rules = map[string]Rule{}
	}
	if cfg.DefaultGPU == "" {
		cfg.DefaultGPU = "igpu"
	}
	return cfg, nil
}

// Save persists config to the default XDG path.
func (c *Config) Save() error {
	p, err := ConfigPath()
	if err != nil {
		return err
	}
	return c.SaveTo(p)
}

// SaveTo writes config atomically to path.
func (c *Config) SaveTo(path string) error {
	if c.Rules == nil {
		c.Rules = map[string]Rule{}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// NormalizeKey normalizes a process name, path, or desktop ID for consistent rule lookup.
func NormalizeKey(target string) string {
	t := strings.TrimSpace(target)
	if strings.Contains(t, "/") {
		t = filepath.Base(t)
	}
	t = strings.TrimSuffix(t, ".desktop")
	return strings.ToLower(t)
}

// SetRule inserts or updates a rule. If another rule shares the same DesktopID,
// it is replaced so conflicting rules for the same desktop application do not accumulate.
func (c *Config) SetRule(rule Rule) {
	if c.Rules == nil {
		c.Rules = map[string]Rule{}
	}
	key := NormalizeKey(rule.Target)
	rule.Target = key
	if rule.DisplayName == "" {
		rule.DisplayName = key
	}
	if rule.DesktopID != "" {
		normDesktop := NormalizeKey(rule.DesktopID)
		for k, existing := range c.Rules {
			if k == key {
				continue
			}
			if (existing.DesktopID != "" && NormalizeKey(existing.DesktopID) == normDesktop) || k == normDesktop {
				delete(c.Rules, k)
			}
		}
	}
	c.Rules[key] = rule
}

// RemoveRule removes a rule by target or desktop ID, returning the removed rule if found.
func (c *Config) RemoveRule(target string) (Rule, bool) {
	key := NormalizeKey(target)
	if r, ok := c.Rules[key]; ok {
		delete(c.Rules, key)
		return r, true
	}
	for k, r := range c.Rules {
		if r.DesktopID != "" && NormalizeKey(r.DesktopID) == key {
			delete(c.Rules, k)
			return r, true
		}
	}
	return Rule{}, false
}

// MatchRule finds a rule matching a process executable name, binary path, or desktop ID.
func (c *Config) MatchRule(candidates ...string) (Rule, bool) {
	for _, cand := range candidates {
		if strings.TrimSpace(cand) == "" {
			continue
		}
		key := NormalizeKey(cand)
		if r, ok := c.Rules[key]; ok {
			return r, true
		}
		for _, r := range c.Rules {
			if r.DesktopID != "" && NormalizeKey(r.DesktopID) == key {
				return r, true
			}
		}
	}
	return Rule{}, false
}

// SortedRules returns all rules sorted alphabetically by target.
func (c *Config) SortedRules() []Rule {
	out := make([]Rule, 0, len(c.Rules))
	for _, r := range c.Rules {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Target < out[j].Target
	})
	return out
}
