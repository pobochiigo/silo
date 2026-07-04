package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestGeneratedCodeCompiles generates middleware for interfaces exercising the
// shapes that are easy to get wrong (no-context methods, non-error results,
// variadic parameters, receiver-colliding parameter names, embedded
// interfaces, struct zero values) and verifies the output actually compiles.
func TestGeneratedCodeCompiles(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping compile check in -short mode")
	}

	cwd, err := os.Getwd()
	require.NoError(t, err)
	moduleRoot, err := findModuleRoot(cwd)
	require.NoError(t, err)

	tmp := t.TempDir()

	// Build the generator binary once.
	genBin := filepath.Join(tmp, "middlegen")
	out, err := exec.Command("go", "build", "-o", genBin, ".").CombinedOutput()
	require.NoError(t, err, "building middlegen: %s", out)

	// Scaffold a consumer module that depends on silo via a replace directive.
	goMod := `module example.com/genverify

go 1.26.0

require github.com/pobochiigo/silo v0.0.0

replace github.com/pobochiigo/silo => ` + moduleRoot + "\n"
	require.NoError(t, os.WriteFile(filepath.Join(tmp, "go.mod"), []byte(goMod), 0o600))

	repoSrc := `package repo

import (
	"context"
	"io"
)

type Thing struct{ ID string }

type Repo interface {
	io.Closer // embedded interfaces are skipped, not wrapped

	// no context, returns error
	Ping() error

	// context, single non-error result
	Name(ctx context.Context) string

	// variadic parameters
	SaveAll(ctx context.Context, things ...*Thing) error

	// parameter name collides with template receivers, ctx param not named ctx
	Create(c context.Context, t *Thing, m string) (*Thing, error)

	// struct result with no matching parameter (zero-value synthesis)
	//middlegen:non-transactional
	Resolve(ctx context.Context) (Thing, error)

	// multiple results with trailing error
	Fetch(ctx context.Context, id string) (string, int, error)
}
`
	serviceSrc := `package service

import "context"

type Service interface {
	Ping() error
	Rename(ctx context.Context, name string) string
	Fire(ctx context.Context)
	Register(ctx context.Context, name string) (string, error)
	Stats(ctx context.Context) (int, bool, error)
}
`
	require.NoError(t, os.MkdirAll(filepath.Join(tmp, "repo"), 0o750))
	require.NoError(t, os.MkdirAll(filepath.Join(tmp, "service"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(tmp, "repo", "repo.go"), []byte(repoSrc), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(tmp, "service", "service.go"), []byte(serviceSrc), 0o600))

	runIn := func(dir string, name string, args ...string) {
		t.Helper()
		cmd := exec.Command(name, args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GOFLAGS=-mod=mod")
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, "%s %v in %s:\n%s", name, args, dir, out)
	}

	runIn(filepath.Join(tmp, "repo"), genBin, "-type=Repo", "-kinds=logging,tracing,metrics,uow_repo")
	runIn(filepath.Join(tmp, "service"), genBin, "-type=Service", "-kinds=logging,tracing,metrics,uow_service")

	runIn(tmp, "go", "mod", "tidy")
	runIn(tmp, "go", "build", "./...")
	runIn(tmp, "go", "vet", "./...")
}
