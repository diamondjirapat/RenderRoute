package config

import (
	"path/filepath"
	"testing"
)

func TestConfigPersistenceAndRuleMatching(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "renderroute", "config.json")

	cfg, err := LoadFrom(cfgPath)
	if err != nil {
		t.Fatalf("LoadFrom missing file failed: %v", err)
	}
	if cfg.DefaultGPU != "igpu" {
		t.Fatalf("expected default igpu, got %q", cfg.DefaultGPU)
	}

	cfg.DefaultGPU = "dgpu"
	cfg.SetRule(Rule{
		Target:    "/usr/bin/Blender",
		GPU:       "dgpu",
		DesktopID: "org.blender.Blender",
	})
	if err := cfg.SaveTo(cfgPath); err != nil {
		t.Fatalf("SaveTo failed: %v", err)
	}

	loaded, err := LoadFrom(cfgPath)
	if err != nil {
		t.Fatalf("LoadFrom saved file failed: %v", err)
	}
	if loaded.DefaultGPU != "dgpu" {
		t.Errorf("expected default dgpu, got %q", loaded.DefaultGPU)
	}

	r, ok := loaded.MatchRule("blender")
	if !ok || r.GPU != "dgpu" {
		t.Errorf("expected match by binary name, got %+v, %v", r, ok)
	}
	r2, ok2 := loaded.MatchRule("org.blender.Blender.desktop")
	if !ok2 || r2.Target != "blender" {
		t.Errorf("expected match by desktop ID, got %+v, %v", r2, ok2)
	}

	if _, removed := loaded.RemoveRule("org.blender.Blender"); !removed {
		t.Errorf("expected RemoveRule by desktop ID to succeed")
	}
	if len(loaded.SortedRules()) != 0 {
		t.Errorf("expected 0 rules after removal")
	}
}
