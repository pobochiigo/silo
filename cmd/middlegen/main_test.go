package main

import (
	"flag"
	"go/ast"
	"go/types"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var update = flag.Bool("update", false, "rewrite the golden files under testdata/golden")

func TestToSnakeCase(t *testing.T) {
	testCases := []struct {
		input    string
		expected string
	}{
		{"UserRepository", "user_repository"},
		{"UserService", "user_service"},
		{"GetByID", "get_by_id"},
		{"HTTPClient", "http_client"},
		{"OAuth2Provider", "o_auth2_provider"},
		{"User", "user"},
		{"user", "user"},
	}

	for _, tc := range testCases {
		assert.Equal(t, tc.expected, toSnakeCase(tc.input), tc.input)
	}
}

func TestToCamelCase(t *testing.T) {
	testCases := []struct {
		input    string
		expected string
	}{
		{"some_metric_name", "someMetricName"},
		{"hello", "hello"},
		{"foo_bar", "fooBar"},
	}

	for _, tc := range testCases {
		assert.Equal(t, tc.expected, toCamelCase(tc.input))
	}
}

func TestGetZeroValue(t *testing.T) {
	testCases := []struct {
		input    string
		expected string
	}{
		{"error", "nil"},
		{"*User", "nil"},
		{"[]string", "nil"},
		{"map[string]int", "nil"},
		{"chan int", "nil"},
		{"func(int) error", "nil"},
		{"any", "nil"},
		{"interface{}", "nil"},
		{"string", `""`},
		{"bool", "false"},
		{"int", "0"},
		{"uint64", "0"},
		{"float32", "0.0"},
		{"float64", "0.0"},
		{"myPackage.CustomType", "*new(myPackage.CustomType)"},
		{"CustomStruct", "*new(CustomStruct)"},
	}

	for _, tc := range testCases {
		assert.Equal(t, tc.expected, getZeroValue(tc.input), tc.input)
	}
}

func TestParseDirectives(t *testing.T) {
	doc := &ast.CommentGroup{
		List: []*ast.Comment{
			{Text: "// Save stores the user."},
			{Text: "//middlegen:non-transactional"},
			{Text: "//middlegen:echo user, other"},
			{Text: "//middlegen:redact password token"},
			{Text: "//middlegen:metric attr:user_id = p1"},
			{Text: "//middlegen:metric counter:my_counter"},
			{Text: "//gen:redact ignored-other-prefix"},
		},
	}
	line := &ast.CommentGroup{
		List: []*ast.Comment{
			{Text: "//middlegen:metric attr:tenant_id = t.ID"},
		},
	}

	d := parseDirectives(doc, line, "middlegen")
	assert.True(t, d.nonTransactional)
	assert.Equal(t, []string{"user", "other"}, d.echo)
	assert.False(t, d.echoNone)
	assert.Equal(t, map[string]bool{"password": true, "token": true}, d.redact)
	require.Len(t, d.attrs, 2)
	assert.Equal(t, Field{Name: "user_id", Type: "p1"}, d.attrs[0])
	assert.Equal(t, Field{Name: "tenant_id", Type: "t.ID"}, d.attrs[1])
	assert.Equal(t, []string{"my_counter"}, d.counters)

	none := parseDirectives(&ast.CommentGroup{List: []*ast.Comment{{Text: "//middlegen:echo none"}}}, nil, "middlegen")
	assert.True(t, none.echoNone)
	assert.Empty(t, none.echo)
}

func TestSanitizeParamName(t *testing.T) {
	for _, reserved := range []string{"ok", "time", "uow", "slog", "err", "ctx", "manager", "r0", "r12"} {
		assert.Equal(t, reserved+"Arg", sanitizeParamName(reserved), reserved)
	}
	assert.Equal(t, "user", sanitizeParamName("user"))
	assert.Equal(t, "r", sanitizeParamName("r"))
}

func TestRewriteIdentifiers(t *testing.T) {
	m := Method{Params: []Field{
		{Name: "ctx", Label: "c", IsContext: true},
		{Name: "tArg", Label: "t", Type: "*Thing"},
		{Name: "user", Label: "user", Type: "string"},
	}}

	assert.Equal(t, "tArg.ID", m.rewriteIdentifiers("t.ID"))
	assert.Equal(t, "x.t", m.rewriteIdentifiers("x.t"), "selector fields are not parameters")
	assert.Equal(t, "len(tArg.ID)+len(user)", m.rewriteIdentifiers("len(t.ID)+len(user)"))
	assert.Equal(t, "tenant", m.rewriteIdentifiers("tenant"), "prefix of a longer identifier is untouched")
}

func TestImportSet(t *testing.T) {
	dest := types.NewPackage("example.com/app/repo", "repo")
	s := newImportSet(dest.Path(), map[string]string{
		"context":                        "context",
		"go.opentelemetry.io/otel/trace": "trace",
	})

	assert.Equal(t, "", s.qualifier(dest), "destination package needs no qualifier")
	assert.Equal(t, "", s.qualifier(nil))

	pb := types.NewPackage("example.com/app/pb", "pb")
	assert.Equal(t, "pb", s.qualifier(pb))
	assert.Equal(t, "pb", s.qualifier(pb), "stable across calls")
	assert.Equal(t, "pb.Foo", s.qualifiedName(pb, "Foo"))
	assert.Equal(t, "Foo", s.qualifiedName(dest, "Foo"))

	otherPB := types.NewPackage("example.com/other/pb", "pb")
	assert.Equal(t, "pb2", s.qualifier(otherPB), "same package name, different path")

	rt := types.NewPackage("runtime/trace", "trace")
	assert.Equal(t, "trace2", s.qualifier(rt), "names of template imports are reserved")

	otelTrace := types.NewPackage("go.opentelemetry.io/otel/trace", "trace")
	assert.Equal(t, "trace", s.qualifier(otelTrace), "same path as a template import reuses its name")

	pgx := types.NewPackage("github.com/jackc/pgx/v5", "pgx")
	assert.Equal(t, "pgx", s.qualifier(pgx))

	uowPkg := types.NewPackage("example.com/x/uow", "uow")
	assert.Equal(t, "uow2", s.qualifier(uowPkg), "reserved local identifiers are avoided")

	assert.True(t, s.isName("pb2"))
	assert.False(t, s.isName("context"), "fixed but unused imports are not qualifiers in play")

	specs := s.specs(map[string]bool{"go.opentelemetry.io/otel/trace": true})
	assert.Equal(t, []string{
		`"example.com/app/pb"`,
		`pb2 "example.com/other/pb"`,
		`uow2 "example.com/x/uow"`,
		`pgx "github.com/jackc/pgx/v5"`,
		`trace2 "runtime/trace"`,
	}, specs, "sorted by path, aliased when the qualifier differs from the path's last element")
}

func TestEchoStatement(t *testing.T) {
	thing := func(params ...Field) Method {
		return Method{Name: "M", Params: append([]Field{{Name: "ctx", Label: "ctx", Type: "context.Context", IsContext: true}}, params...)}
	}
	errResult := Field{Name: "err", Type: "error", IsError: true}
	noDirectives := methodDirectives{redact: map[string]bool{}}

	t.Run("no results", func(t *testing.T) {
		stmt, err := thing().echoStatement(noDirectives, "I", "middlegen")
		require.NoError(t, err)
		assert.Equal(t, "return", stmt)
	})

	t.Run("unique entity parameter is echoed", func(t *testing.T) {
		m := thing(Field{Name: "u", Label: "u", Type: "*User"})
		m.Results = []Field{{Name: "r0", Type: "*User"}, errResult}
		stmt, err := m.echoStatement(noDirectives, "I", "middlegen")
		require.NoError(t, err)
		assert.Equal(t, "return u, nil", stmt)
	})

	t.Run("basic types are never echoed", func(t *testing.T) {
		m := thing(Field{Name: "id", Label: "id", Type: "string"})
		m.Results = []Field{{Name: "r0", Type: "string"}, {Name: "r1", Type: "int"}, errResult}
		stmt, err := m.echoStatement(noDirectives, "I", "middlegen")
		require.NoError(t, err)
		assert.Equal(t, `return "", 0, nil`, stmt)
	})

	t.Run("variadic parameters are never echoed", func(t *testing.T) {
		m := thing(Field{Name: "items", Label: "items", Type: "...*User", Variadic: true})
		m.Results = []Field{{Name: "r0", Type: "[]*User"}, errResult}
		stmt, err := m.echoStatement(noDirectives, "I", "middlegen")
		require.NoError(t, err)
		assert.Equal(t, "return nil, nil", stmt)
	})

	t.Run("dereference is nil guarded", func(t *testing.T) {
		m := thing(Field{Name: "u", Label: "u", Type: "*User"})
		m.Results = []Field{{Name: "r0", Type: "User"}, errResult}
		stmt, err := m.echoStatement(noDirectives, "I", "middlegen")
		require.NoError(t, err)
		assert.Contains(t, stmt, "var r0 User")
		assert.Contains(t, stmt, "if u != nil {")
		assert.Contains(t, stmt, "r0 = *u")
		assert.Contains(t, stmt, "return r0, nil")
	})

	t.Run("ambiguous candidates fall back to zero value", func(t *testing.T) {
		m := thing(Field{Name: "a", Label: "a", Type: "*User"}, Field{Name: "b", Label: "b", Type: "*User"})
		m.Results = []Field{{Name: "r0", Type: "*User"}, errResult}
		stmt, err := m.echoStatement(noDirectives, "I", "middlegen")
		require.NoError(t, err)
		assert.Equal(t, "return nil, nil", stmt)
	})

	t.Run("explicit echo picks a candidate and may take a value address", func(t *testing.T) {
		m := thing(Field{Name: "a", Label: "a", Type: "User"}, Field{Name: "b", Label: "b", Type: "*User"})
		m.Results = []Field{{Name: "r0", Type: "*User"}, errResult}
		stmt, err := m.echoStatement(methodDirectives{echo: []string{"a"}}, "I", "middlegen")
		require.NoError(t, err)
		assert.Equal(t, "return &a, nil", stmt)
	})

	t.Run("explicit echo none", func(t *testing.T) {
		m := thing(Field{Name: "u", Label: "u", Type: "*User"})
		m.Results = []Field{{Name: "r0", Type: "*User"}, errResult}
		stmt, err := m.echoStatement(methodDirectives{echoNone: true}, "I", "middlegen")
		require.NoError(t, err)
		assert.Equal(t, "return nil, nil", stmt)
	})

	t.Run("explicit echo errors", func(t *testing.T) {
		m := thing(Field{Name: "u", Label: "u", Type: "*User"})
		m.Results = []Field{{Name: "r0", Type: "*Order"}, errResult}

		_, err := m.echoStatement(methodDirectives{echo: []string{"missing"}}, "I", "middlegen")
		assert.ErrorContains(t, err, `unknown parameter "missing"`)

		_, err = m.echoStatement(methodDirectives{echo: []string{"u"}}, "I", "middlegen")
		assert.ErrorContains(t, err, "cannot be returned as result 0")

		_, err = m.echoStatement(methodDirectives{echo: []string{"u", "u"}}, "I", "middlegen")
		assert.ErrorContains(t, err, "names 2 parameters but the method has 1")
	})
}

func TestFinalize(t *testing.T) {
	m := Method{
		Name: "Save",
		Params: []Field{
			{Name: "ctx", Label: "ctx", Type: "context.Context", IsContext: true},
			{Name: "tArg", Label: "t", Type: "*Thing"},
			{Name: "secret", Label: "secret", Type: "string", Redact: true},
			{Name: "tags", Label: "tags", Type: "...string", Variadic: true},
		},
		Results: []Field{
			{Name: "r0", Type: "*Thing"},
			{Name: "err", Type: "error", IsError: true},
		},
		HasContext: true,
		HasError:   true,
	}
	m.finalize()

	assert.Equal(t, "ctx context.Context, tArg *Thing, secret string, tags ...string", m.ParamsSignature)
	assert.Equal(t, "ctx, tArg, secret, tags...", m.ParamsNames)
	assert.Equal(t, "txCtx, tArg, secret, tags...", m.ParamsNamesWithTx)
	assert.Equal(t, "uowCtx, tArg, secret, tags...", m.ParamsNamesWithUow)
	assert.Equal(t, `slog.Any("t", tArg), slog.String("secret", "[REDACTED]"), slog.Any("tags", tags)`, m.SlogAttributes)
	assert.Equal(t, "(*Thing, error)", m.ResultsSignature)
	assert.Equal(t, "r0, err", m.ResultsVars)
	assert.Equal(t, "r0", m.NonErrorResultsVars)
	assert.Equal(t, "_, err := m.next.Save(txCtx, tArg, secret, tags...)\n\t\t\treturn err", m.RepoDeferStmt)
}

func TestModuleHelpers(t *testing.T) {
	tempDir := t.TempDir()

	goModContent := `module github.com/my-user/my-project

go 1.22
`
	require.NoError(t, os.WriteFile(filepath.Join(tempDir, "go.mod"), []byte(goModContent), 0o640))

	nestedDir := filepath.Join(tempDir, "pkg", "sub")
	require.NoError(t, os.MkdirAll(nestedDir, 0o750))

	root, err := findModuleRoot(nestedDir)
	assert.NoError(t, err)
	assert.Equal(t, tempDir, root)

	modName, err := getModuleName(tempDir)
	assert.NoError(t, err)
	assert.Equal(t, "github.com/my-user/my-project", modName)

	assert.Equal(t, "github.com/pobochiigo/silo", detectLibraryModule(tempDir), "foreign modules use the published library")
}

func TestRunValidation(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/g\n\ngo 1.26.0\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "g.go"), []byte(`package g

type Store[T any] interface{ Get() T }

type Plain struct{}
`), 0o600))

	err := run(dir, options{TypeName: "Store", Kinds: []string{"logging"}})
	assert.ErrorContains(t, err, "generic interfaces")

	err = run(dir, options{TypeName: "Plain", Kinds: []string{"logging"}})
	assert.ErrorContains(t, err, "is not an interface")

	err = run(dir, options{TypeName: "Missing", Kinds: []string{"logging"}})
	assert.ErrorContains(t, err, "not found")

	err = run(dir, options{TypeName: "Store", Kinds: []string{"nope"}})
	assert.ErrorContains(t, err, "unknown middleware kind")

	err = run(dir, options{TypeName: "pkg.Store", Kinds: []string{"logging"}})
	assert.ErrorContains(t, err, "bare interface name")

	err = run(dir, options{Kinds: []string{"logging"}})
	assert.ErrorContains(t, err, "-type flag is required")
}

// TestGolden renders every template for the interface in
// testdata/golden/input and compares the output with the checked-in files.
// Run with -update to accept new output after an intentional template change.
func TestGolden(t *testing.T) {
	cwd, err := os.Getwd()
	require.NoError(t, err)
	inputDir := filepath.Join(cwd, "testdata", "golden", "input")
	goldenDir := filepath.Join(cwd, "testdata", "golden")

	for _, kind := range []string{"logging", "tracing", "metrics", "uow_repo", "uow_service"} {
		t.Run(kind, func(t *testing.T) {
			outDir := t.TempDir()
			err := run(inputDir, options{
				TypeName: "Example",
				Kinds:    []string{kind},
				OutDir:   outDir,
			})
			require.NoError(t, err)

			entries, err := os.ReadDir(outDir)
			require.NoError(t, err)
			require.Len(t, entries, 1)
			got, err := os.ReadFile(filepath.Join(outDir, entries[0].Name()))
			require.NoError(t, err)

			goldenFile := filepath.Join(goldenDir, kind+".golden")
			if *update {
				require.NoError(t, os.WriteFile(goldenFile, got, 0o644))
			}
			want, err := os.ReadFile(goldenFile)
			require.NoError(t, err, "missing golden file; run: go test ./cmd/middlegen -run TestGolden -update")
			assert.Equal(t, string(want), string(got))
		})
	}
}
