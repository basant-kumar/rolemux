package catalog

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/basant-kumar/rolemux/internal/config"
	"github.com/basant-kumar/rolemux/internal/runner"
)

type catalogAdapter struct {
	account string
	page    runner.ModelPage
	err     error
	calls   int
}

func (f *catalogAdapter) Run(context.Context, runner.Request, runner.Callbacks) (runner.Response, error) {
	return runner.Response{}, errors.New("not used")
}
func (f *catalogAdapter) ListModels(context.Context, runner.ModelListRequest) (runner.ModelPage, error) {
	f.calls++
	return f.page, f.err
}
func (f *catalogAdapter) Version(context.Context) (string, error) { return "test", nil }
func (f *catalogAdapter) Auth(context.Context) (runner.AuthStatus, error) {
	return runner.AuthStatus{Authenticated: true, Account: f.account}, nil
}

func TestCatalogCachesByAccountAndFallsBackAsUnknown(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	fake := &catalogAdapter{account: "alice", page: runner.ModelPage{Models: []runner.ModelInfo{{ID: "model-a", Origin: "live", Availability: "available"}}}}
	cfg := config.Default()
	cfg.CatalogTTLSeconds = 60
	cat := New(map[string]runner.Adapter{"codex": fake}, cfg, filepath.Join(t.TempDir(), "models.json"))
	cat.Now = func() time.Time { return now }
	models, err := cat.Models(context.Background(), "codex", true, runner.ModelListRequest{})
	if err != nil || len(models) != 1 || models[0].Origin != "live" {
		t.Fatalf("live models=%#v err=%v", models, err)
	}
	fake.err = errors.New("offline")
	now = now.Add(10 * time.Second)
	models, err = cat.Models(context.Background(), "codex", true, runner.ModelListRequest{})
	if err != nil || models[0].Origin != "cache" || models[0].Availability != "unknown" || models[0].AgeSeconds != 10 {
		t.Fatalf("fallback models=%#v err=%v", models, err)
	}
	fake.account = "bob"
	if _, err := cat.Models(context.Background(), "codex", true, runner.ModelListRequest{}); err == nil {
		t.Fatal("cache from another account was reused")
	}
}

func TestCatalogPreservesUnknownClaudeAvailability(t *testing.T) {
	fake := &catalogAdapter{account: "alice", page: runner.ModelPage{Models: []runner.ModelInfo{{ID: "claude-fable-5", Origin: "custom", Availability: "unknown", Custom: true}}}}
	cat := New(map[string]runner.Adapter{"claude": fake}, config.Default(), filepath.Join(t.TempDir(), "models.json"))
	models, err := cat.Models(context.Background(), "claude", true, runner.ModelListRequest{})
	if err != nil || len(models) != 1 {
		t.Fatalf("models=%#v err=%v", models, err)
	}
	if models[0].Origin != "custom" || models[0].Availability != "unknown" || !models[0].Custom {
		t.Fatalf("Claude alias was falsely promoted to live availability: %#v", models[0])
	}
}
