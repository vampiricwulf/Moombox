package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestValidateReportsErrorsWithoutMutating(t *testing.T) {
	cfg := Defaults()
	cfg.Network.Port = 70000
	cfg.Network.NetworkAccess = "bogus"
	cfg.Logs.LogLevel = "LOUD"

	before := cfg.Network.Port
	errs := Validate(cfg)
	if len(errs) == 0 {
		t.Fatal("expected errors for invalid config")
	}
	if cfg.Network.Port != before {
		t.Errorf("Validate should not mutate; Port %d != before %d", cfg.Network.Port, before)
	}

	// Spot-check one joined message
	joined := errors.Join(errs...).Error()
	if !strings.Contains(joined, "network.port") {
		t.Errorf("expected joined error to mention network.port; got %q", joined)
	}
}

func TestNormalizeAppliesDefaultsWithoutErrors(t *testing.T) {
	cfg := Defaults()
	cfg.Network.Port = 70000
	cfg.Logs.LogLevel = "LOUD"
	cfg.Disk.WarnPercent = 150

	Normalize(cfg)

	defaults := Defaults()
	if cfg.Network.Port != defaults.Network.Port {
		t.Errorf("Port: want %d after normalize, got %d", defaults.Network.Port, cfg.Network.Port)
	}
	if cfg.Logs.LogLevel != defaults.Logs.LogLevel {
		t.Errorf("LogLevel: want %q after normalize, got %q", defaults.Logs.LogLevel, cfg.Logs.LogLevel)
	}
	if cfg.Disk.WarnPercent != defaults.Disk.WarnPercent {
		t.Errorf("WarnPercent: want %d after normalize, got %d", defaults.Disk.WarnPercent, cfg.Disk.WarnPercent)
	}
}

func TestValidateClean(t *testing.T) {
	cfg := Defaults()
	if errs := Validate(cfg); len(errs) != 0 {
		t.Errorf("expected no errors on a default config, got %v", errs)
	}
}

func TestStoreReadAccessesCurrent(t *testing.T) {
	cfg := Defaults()
	cfg.Network.Port = 8080
	s := NewStore(cfg, "")

	var port int
	s.Read(func(c *MoomboxConfig) { port = c.Network.Port })
	if port != 8080 {
		t.Errorf("Read Port: want 8080, got %d", port)
	}
}

func TestStoreUpdateAppliesAndValidates(t *testing.T) {
	cfg := Defaults()
	s := NewStore(cfg, "")

	err := s.Update(func(c *MoomboxConfig) {
		c.Network.Port = 9090
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if cfg.Network.Port != 9090 {
		t.Errorf("Port: want 9090, got %d", cfg.Network.Port)
	}
}

func TestStoreUpdateRejectsInvalid(t *testing.T) {
	cfg := Defaults()
	s := NewStore(cfg, "")

	err := s.Update(func(c *MoomboxConfig) {
		c.Network.Port = 70000 // out of range
	})
	if err == nil {
		t.Fatal("expected validation error for port 70000")
	}
	if !strings.Contains(err.Error(), "network.port") {
		t.Errorf("expected error about network.port, got %q", err.Error())
	}
	// No auto-rollback contract — port is mutated even though Save was
	// skipped. Callers wanting transactional semantics must deep-copy.
	if cfg.Network.Port != 70000 {
		t.Errorf("fn's mutation should remain visible; got %d", cfg.Network.Port)
	}
}

func TestStoreUpdateWritesToDisk(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	cfg := Defaults()
	s := NewStore(cfg, path)

	err := s.Update(func(c *MoomboxConfig) {
		c.Network.Port = 8090
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("expected file at %s: %v", path, err)
	}

	// Reload and confirm port persisted
	reloaded, err := loadFromFile(path)
	if err != nil {
		t.Fatalf("loadFromFile: %v", err)
	}
	if reloaded.Network.Port != 8090 {
		t.Errorf("reloaded Port: want 8090, got %d", reloaded.Network.Port)
	}
}

func TestStoreUpdateSkipsSaveOnEmptyPath(t *testing.T) {
	cfg := Defaults()
	s := NewStore(cfg, "") // no save path

	err := s.Update(func(c *MoomboxConfig) {
		c.Network.Port = 9191
	})
	if err != nil {
		t.Fatalf("Update with empty savePath should not error: %v", err)
	}
}

func TestStoreConcurrentReadAndUpdate(t *testing.T) {
	// Race test: many readers calling Read while one writer calls Update.
	// The embedded RWMutex serializes updates against ongoing reads; field
	// access happens inside Read's RLock scope. Run under -race to verify.
	cfg := Defaults()
	s := NewStore(cfg, "")

	var wg sync.WaitGroup
	stop := make(chan struct{})
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				s.Read(func(c *MoomboxConfig) { _ = c.Network.Port })
			}
		}()
	}

	for i := range 50 {
		if err := s.Update(func(c *MoomboxConfig) {
			c.Network.Port = 8000 + i
		}); err != nil {
			t.Fatalf("Update: %v", err)
		}
	}
	close(stop)
	wg.Wait()
}

func TestStoreRWMutexLegacyAccess(t *testing.T) {
	cfg := Defaults()
	s := NewStore(cfg, "")

	// Legacy call site: manual RLock + direct field read.
	s.RWMutex().RLock()
	port := s.Config().Network.Port
	s.RWMutex().RUnlock()
	if port != Defaults().Network.Port {
		t.Errorf("legacy access: want %d, got %d", Defaults().Network.Port, port)
	}
}

func TestStoreSaveLockedWritesToDisk(t *testing.T) {
	// Caller-locked save is the migration bridge for write-with-rollback
	// route handlers. The caller holds the write lock, mutates the config,
	// calls SaveLocked, and rolls back the in-memory mutation if SaveLocked
	// returns an error — preserving the cfgMu+saveConfig transactional
	// pattern while routing through the Store's lock + savePath.
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	cfg := Defaults()
	s := NewStore(cfg, path)

	s.RWMutex().Lock()
	cfg.Network.Port = 8765
	err := s.SaveLocked()
	s.RWMutex().Unlock()
	if err != nil {
		t.Fatalf("SaveLocked: %v", err)
	}

	reloaded, err := loadFromFile(path)
	if err != nil {
		t.Fatalf("loadFromFile: %v", err)
	}
	if reloaded.Network.Port != 8765 {
		t.Errorf("reloaded Port: want 8765, got %d", reloaded.Network.Port)
	}
}

func TestStoreSaveLockedNoOpOnEmptyPath(t *testing.T) {
	cfg := Defaults()
	s := NewStore(cfg, "") // no save path

	s.RWMutex().Lock()
	defer s.RWMutex().Unlock()
	if err := s.SaveLocked(); err != nil {
		t.Errorf("SaveLocked with empty savePath should be a no-op, got %v", err)
	}
}
