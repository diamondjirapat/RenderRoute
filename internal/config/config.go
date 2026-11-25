package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
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
// It never returns a nil *Config: on error, defaults are returned alongside the error so
// read-only callers can keep working while mutating callers can refuse to overwrite the file.
func Load() (*Config, error) {
	p, err := ConfigPath()
	if err != nil {
		return Default(), err
	}
	return LoadFrom(p)
}

// LoadFrom reads config from a specific file path, returning defaults if it doesn't exist.
// It never returns a nil *Config; on read/parse errors it returns defaults plus the error.
func LoadFrom(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Default(), nil
		}
		return Default(), fmt.Errorf("read config %s: %w", path, err)
	}
	cfg := Default()
	if err := json.Unmarshal(b, cfg); err != nil {
		return Default(), fmt.Errorf("parse config %s: %w", path, err)
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

// SaveTo writes config atomically to path (unique temp file + fsync + rename).
func (c *Config) SaveTo(path string) error {
	if c.Rules == nil {
		c.Rules = map[string]Rule{}
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Chmod(0o644); err != nil {
		tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		cleanup()
		return err
	}
	return nil
}

// Lock takes an exclusive advisory lock guarding read-modify-write cycles on the default
// config file, so concurrent renderroute invocations (e.g. from the DMS widget) don't
// overwrite each other's changes. Call the returned function to release it.
func Lock() (func(), error) {
	p, err := ConfigPath()
	if err != nil {
		return func() {}, err
	}
	return LockPath(p)
}

// LockPath takes an exclusive advisory lock on "<path>.lock".
func LockPath(path string) (func(), error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return func() {}, err
	}
	f, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return func() {}, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return func() {}, fmt.Errorf("lock config %s: %w", path, err)
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, nil
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
