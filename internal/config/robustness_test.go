package config

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestLoadFromCorruptConfigReturnsDefaultsWithError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"default_gpu": "dgpu", "rules": {`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadFrom(path)
	if err == nil {
		t.Fatalf("expected parse error for truncated config")
	}
	if cfg == nil {
		t.Fatalf("LoadFrom must never return a nil config")
	}
	if cfg.DefaultGPU != "igpu" || cfg.Rules == nil {
		t.Errorf("expected defaults on parse error, got %+v", cfg)
	}
	// Default config must be safe to query.
	if _, ok := cfg.MatchRule("anything"); ok {
		t.Errorf("unexpected rule match on default config")
	}
}

func TestLockedReadModifyWriteDoesNotLoseUpdates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "renderroute", "config.json")
	const writers = 16
	var wg sync.WaitGroup
	errs := make(chan error, writers)
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			unlock, err := LockPath(path)
			if err != nil {
				errs <- err
				return
			}
			defer unlock()
			cfg, err := LoadFrom(path)
			if err != nil {
				errs <- err
				return
			}
			cfg.SetRule(Rule{Target: fmt.Sprintf("app%d", i), GPU: "dgpu"})
			if err := cfg.SaveTo(path); err != nil {
				errs <- err
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent update failed: %v", err)
	}
	cfg, err := LoadFrom(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(cfg.Rules); got != writers {
		t.Errorf("expected %d rules after concurrent locked updates, got %d", writers, got)
	}
	leftovers, _ := filepath.Glob(filepath.Join(filepath.Dir(path), "*.tmp"))
	if len(leftovers) != 0 {
		t.Errorf("temp files left behind: %v", leftovers)
	}
}
