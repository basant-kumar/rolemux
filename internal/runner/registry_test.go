package runner

import (
	"context"
	"reflect"
	"testing"

	"github.com/basant/rolemux/internal/provider"
)

type registryAdapter struct{}

func (registryAdapter) Run(context.Context, Request, Callbacks) (Response, error) {
	return Response{}, nil
}
func (registryAdapter) ListModels(context.Context, ModelListRequest) (ModelPage, error) {
	return ModelPage{}, nil
}
func (registryAdapter) Version(context.Context) (string, error)  { return "", nil }
func (registryAdapter) Auth(context.Context) (AuthStatus, error) { return AuthStatus{}, nil }

func TestRegistryExtendsWithoutCommandChanges(t *testing.T) {
	r := NewRegistry()
	if err := r.Register("future-cli", func(path, root string) (Adapter, string, error) {
		return registryAdapter{}, path + root, nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := r.Register("future-cli", func(string, string) (Adapter, string, error) { return registryAdapter{}, "", nil }); err == nil {
		t.Fatal("duplicate registration accepted")
	}
	adapter, resolved, err := r.Build("FUTURE-CLI", "/bin/tool", "/repo")
	if err != nil || adapter == nil || resolved != "/bin/tool/repo" {
		t.Fatalf("build adapter=%T path=%q err=%v", adapter, resolved, err)
	}
	if got := r.Names(); !reflect.DeepEqual(got, []string{"future-cli"}) {
		t.Fatalf("names=%v", got)
	}
}

func TestBuiltinRegistryMatchesSupportedProviderIDs(t *testing.T) {
	if got, want := BuiltinRegistry().Names(), provider.Names(); !reflect.DeepEqual(got, want) {
		t.Fatalf("registered providers=%v, configured providers=%v", got, want)
	}
}
