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
// variadic parameters, receiver- and package-colliding parameter names,
// embedded interfaces, struct zero values, import pruning) and verifies that
// the output compiles and behaves: the scaffolded module carries tests that
// call the generated wrappers.
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

go 1.27.1

require github.com/pobochiigo/silo v0.0.0

replace github.com/pobochiigo/silo => ` + moduleRoot + "\n"
	require.NoError(t, os.WriteFile(filepath.Join(tmp, "go.mod"), []byte(goMod), 0o600))

	files := map[string]string{
		"repo/repo.go":          repoSrc,
		"repo/probe_test.go":    repoProbeSrc,
		"service/service.go":    serviceSrc,
		"service/probe_test.go": serviceProbeSrc,
	}
	for name, src := range files {
		require.NoError(t, os.MkdirAll(filepath.Dir(filepath.Join(tmp, name)), 0o750))
		require.NoError(t, os.WriteFile(filepath.Join(tmp, name), []byte(src), 0o600))
	}

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
	runIn(tmp, "go", "vet", "./...")
	runIn(tmp, "go", "test", "./...")
}

const repoSrc = `package repo

import (
	"bufio"
	"context"
	"database/sql/driver"
	"io"
	"time"
)

type Thing struct{ ID string }

// Base is embedded by Repo; its directive must apply to the promoted method.
type Base interface {
	//middlegen:non-transactional
	Exists(ctx context.Context, id string) (bool, error)
}

type Repo interface {
	io.Closer     // standard library embedded interface
	driver.Valuer // embedded method with a type from another package
	Base          // same-package embedded interface

	// no context, returns error
	Ping() error

	// blank parameter name, context not in first position
	Tag(_ string, ctx context.Context) error

	// context, single non-error result
	Name(ctx context.Context) string

	// context, no results at all
	Touch(ctx context.Context)

	// variadic parameters
	SaveAll(ctx context.Context, things ...*Thing) error

	// parameter names collide with template receivers, ctx param not named ctx;
	// the single *Thing parameter is echoed back when deferred
	Create(c context.Context, t *Thing, m string) (*Thing, error)

	// struct result with no matching parameter (zero-value synthesis)
	//middlegen:non-transactional
	Resolve(ctx context.Context) (Thing, error)

	// basic-typed results are never echoed from parameters
	Fetch(ctx context.Context, id string) (string, int, error)

	// parameter named like the local the uow template declares
	SetFlag(ctx context.Context, ok bool) error

	// parameter shadows a package the templates use
	Schedule(ctx context.Context, time time.Time) error

	// value result from pointer parameter: dereference must be nil-safe
	Upsert(ctx context.Context, u *Thing) (Thing, error)

	// two candidates for the result: ambiguous, so the zero value is returned
	Move(ctx context.Context, src *Thing, dst *Thing) (*Thing, error)

	// explicit choice among candidates
	//middlegen:echo dst
	Pick(ctx context.Context, src *Thing, dst *Thing) (*Thing, error)

	// echoing disabled
	//middlegen:echo none
	Clone(ctx context.Context, src *Thing) (*Thing, error)

	// secrets must not be logged
	//middlegen:redact password
	Login(ctx context.Context, user string, password string) error

	// import pruning: the file imports io and bufio; only bufio is used here
	Read(ctx context.Context, r *bufio.Reader) error
}
`

const repoProbeSrc = `package repo

import (
	"bufio"
	"bytes"
	"context"
	"database/sql/driver"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/pobochiigo/silo/uow"
)

type spy struct {
	calls map[string]int
	flag  bool
	tag   string
}

func newSpy() *spy { return &spy{calls: map[string]int{}} }

func (s *spy) Close() error                                  { s.calls["Close"]++; return nil }
func (s *spy) Value() (driver.Value, error)                  { s.calls["Value"]++; return "v", nil }
func (s *spy) Exists(context.Context, string) (bool, error)  { s.calls["Exists"]++; return true, nil }
func (s *spy) Tag(tag string, _ context.Context) error       { s.calls["Tag"]++; s.tag = tag; return nil }
func (s *spy) Ping() error                                   { s.calls["Ping"]++; return nil }
func (s *spy) Name(context.Context) string                   { s.calls["Name"]++; return "spy" }
func (s *spy) Touch(context.Context)                         { s.calls["Touch"]++ }
func (s *spy) SaveAll(context.Context, ...*Thing) error      { s.calls["SaveAll"]++; return nil }
func (s *spy) Resolve(context.Context) (Thing, error)        { s.calls["Resolve"]++; return Thing{ID: "resolved"}, nil }
func (s *spy) Fetch(context.Context, string) (string, int, error) {
	s.calls["Fetch"]++
	return "fetched", 7, nil
}
func (s *spy) SetFlag(_ context.Context, ok bool) error      { s.calls["SetFlag"]++; s.flag = ok; return nil }
func (s *spy) Schedule(context.Context, time.Time) error     { s.calls["Schedule"]++; return nil }
func (s *spy) Upsert(_ context.Context, u *Thing) (Thing, error) {
	s.calls["Upsert"]++
	return *u, nil
}
func (s *spy) Create(_ context.Context, t *Thing, _ string) (*Thing, error) {
	s.calls["Create"]++
	return &Thing{ID: "db-" + t.ID}, nil
}
func (s *spy) Move(context.Context, *Thing, *Thing) (*Thing, error)  { s.calls["Move"]++; return nil, nil }
func (s *spy) Pick(context.Context, *Thing, *Thing) (*Thing, error)  { s.calls["Pick"]++; return nil, nil }
func (s *spy) Clone(context.Context, *Thing) (*Thing, error)         { s.calls["Clone"]++; return nil, nil }
func (s *spy) Login(context.Context, string, string) error           { s.calls["Login"]++; return nil }
func (s *spy) Read(context.Context, *bufio.Reader) error             { s.calls["Read"]++; return nil }

type fakeTx struct{}

func (fakeTx) Commit(context.Context) error   { return nil }
func (fakeTx) Rollback(context.Context) error { return nil }

type fakeTransactor struct{}

func (fakeTransactor) BeginTx(ctx context.Context) (uow.Tx, context.Context, error) {
	return fakeTx{}, ctx, nil
}

func TestDeferredCallForwardsArguments(t *testing.T) {
	s := newSpy()
	r := RepoUoWMiddleware()(s)
	m := uow.NewManager(fakeTransactor{})

	err := m.RunWith(context.Background(), func(ctx context.Context) error {
		if err := r.SetFlag(ctx, true); err != nil {
			return err
		}
		if err := r.SetFlag(ctx, false); err != nil {
			return err
		}
		if err := r.Tag("later", ctx); err != nil {
			return err
		}
		if s.calls["Tag"] != 0 {
			t.Fatal("Tag: a context in second position must still defer the call")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if s.calls["SetFlag"] != 2 {
		t.Fatalf("SetFlag called %d times, want 2", s.calls["SetFlag"])
	}
	if s.flag != false {
		t.Fatalf("last SetFlag received ok=%v, want false (the ok local must not shadow the parameter)", s.flag)
	}
	if s.calls["Tag"] != 1 || s.tag != "later" {
		t.Fatalf("Tag forwarded %q after %d calls, want \"later\" once", s.tag, s.calls["Tag"])
	}
}

func TestEmbeddedInterfacesAreDecorated(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(prev)

	s := newSpy()
	var r Repo = RepoLoggingMiddleware()(RepoUoWMiddleware()(s))
	ctx := uow.Inject(context.Background(), uow.NewUnitOfWork())

	// Promoted from io.Closer and driver.Valuer: decorated, not just forwarded.
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	if v, err := r.Value(); err != nil || v != "v" {
		t.Fatalf("Value: got (%v, %v)", v, err)
	}
	// Promoted from Base with its non-transactional directive: runs immediately.
	if ok, err := r.Exists(ctx, "id"); err != nil || !ok {
		t.Fatalf("Exists: got (%v, %v), want immediate execution", ok, err)
	}
	if s.calls["Close"] != 1 || s.calls["Value"] != 1 || s.calls["Exists"] != 1 {
		t.Fatalf("unexpected calls: %v", s.calls)
	}

	out := buf.String()
	for _, want := range []string{"Close started", "Value started", "Exists started"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in logs:\n%s", want, out)
		}
	}
}

func TestDeferredReturnValues(t *testing.T) {
	s := newSpy()
	r := RepoUoWMiddleware()(s)
	ctx := uow.Inject(context.Background(), uow.NewUnitOfWork())

	in := &Thing{ID: "a"}
	if got, err := r.Create(ctx, in, "x"); err != nil || got != in {
		t.Fatalf("Create: got (%v, %v), want the echoed input %p", got, err, in)
	}

	if v, err := r.Upsert(ctx, nil); err != nil || v != (Thing{}) {
		t.Fatalf("Upsert(nil): got (%v, %v), want zero value without panic", v, err)
	}
	if v, _ := r.Upsert(ctx, in); v.ID != "a" {
		t.Fatalf("Upsert: got %v, want dereferenced input", v)
	}

	if id, n, err := r.Fetch(ctx, "id-1"); id != "" || n != 0 || err != nil {
		t.Fatalf("Fetch: got (%q, %d, %v), want zero values", id, n, err)
	}

	if moved, _ := r.Move(ctx, &Thing{ID: "src"}, &Thing{ID: "dst"}); moved != nil {
		t.Fatalf("Move: got %v, want nil for an ambiguous echo", moved)
	}

	dst := &Thing{ID: "dst"}
	if picked, _ := r.Pick(ctx, &Thing{ID: "src"}, dst); picked != dst {
		t.Fatalf("Pick: got %v, want the parameter named by the echo directive", picked)
	}

	if cloned, _ := r.Clone(ctx, in); cloned != nil {
		t.Fatalf("Clone: got %v, want nil because echoing is disabled", cloned)
	}

	if name := r.Name(ctx); name != "" {
		t.Fatalf("Name: got %q, want zero value while deferred", name)
	}
	r.Touch(ctx)
	if s.calls["Touch"] != 0 {
		t.Fatal("Touch: void methods are deferred too")
	}

	if got, _ := r.Resolve(ctx); got.ID != "resolved" {
		t.Fatalf("Resolve: got %v, want immediate execution (non-transactional)", got)
	}

	if s.calls["Create"]+s.calls["Upsert"]+s.calls["Fetch"]+s.calls["Move"]+s.calls["Pick"]+s.calls["Clone"]+s.calls["Name"] != 0 {
		t.Fatalf("deferred methods must not run before commit: %v", s.calls)
	}
}

func TestLoggingRedaction(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(prev)

	r := RepoLoggingMiddleware()(newSpy())
	if err := r.Login(context.Background(), "alice", "hunter2"); err != nil {
		t.Fatal(err)
	}

	out := buf.String()
	if strings.Contains(out, "hunter2") {
		t.Fatalf("password leaked into logs: %s", out)
	}
	if !strings.Contains(out, "[REDACTED]") || !strings.Contains(out, "alice") {
		t.Fatalf("unexpected log line: %s", out)
	}
}

func TestParameterNamedLikeAPackageStillCompiles(t *testing.T) {
	r := RepoMetricsMiddleware()(newSpy())
	if err := r.Schedule(context.Background(), time.Now()); err != nil {
		t.Fatal(err)
	}
}
`

const serviceSrc = `package service

import "context"

type Service interface {
	Ping() error
	Rename(ctx context.Context, name string) string
	Fire(ctx context.Context)
	Register(ctx context.Context, name string) (string, error)
	Stats(ctx context.Context) (int, bool, error)
}
`

const serviceProbeSrc = `package service

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/pobochiigo/silo/uow"
)

type spy struct{}

func (spy) Ping() error { return nil }
func (spy) Rename(ctx context.Context, name string) string {
	if u, ok := uow.Extract(ctx); ok {
		u.Defer(func(context.Context) error { return nil })
	}
	return name
}
func (spy) Fire(ctx context.Context) {
	if u, ok := uow.Extract(ctx); ok {
		u.Defer(func(context.Context) error { return nil })
	}
}
func (spy) Register(context.Context, string) (string, error) { return "id", nil }
func (spy) Stats(context.Context) (int, bool, error)         { return 1, true, nil }

type failingTransactor struct{}

func (failingTransactor) BeginTx(ctx context.Context) (uow.Tx, context.Context, error) {
	return nil, nil, errors.New("database unreachable")
}

func TestErrorlessMethodsLogUnitOfWorkFailures(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	defer slog.SetDefault(prev)

	svc := ServiceUoWMiddleware(uow.NewManager(failingTransactor{}))(spy{})

	svc.Fire(context.Background())
	if got := svc.Rename(context.Background(), "x"); got != "x" {
		t.Fatalf("Rename returned %q", got)
	}

	out := buf.String()
	if strings.Count(out, "unit of work failed") != 2 || !strings.Contains(out, "database unreachable") {
		t.Fatalf("expected two logged failures, got:\n%s", out)
	}
}
`
