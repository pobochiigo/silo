package main

import (
	"flag"
	"go/ast"
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
		{"any", "nil"},
		{"string", `""`},
		{"bool", "false"},
		{"int", "0"},
		{"float32", "0.0"},
		{"float64", "0.0"},
		{"myPackage.CustomType", "*new(myPackage.CustomType)"},
		{"CustomStruct", "*new(CustomStruct)"},
	}

	for _, tc := range testCases {
		assert.Equal(t, tc.expected, getZeroValue(tc.input))
	}
}

func TestParseMethodComments(t *testing.T) {
	doc := &ast.CommentGroup{
		List: []*ast.Comment{
			{Text: "//middlegen:metric attr:user_id = p1"},
			{Text: "//middlegen:metric counter:my_counter"},
		},
	}
	line := &ast.CommentGroup{
		List: []*ast.Comment{
			{Text: "//middlegen:metric attr:tenant_id = p2"},
		},
	}

	attrs, counters := parseMethodComments(doc, line, "middlegen")

	require.Len(t, attrs, 2)
	assert.Equal(t, "user_id", attrs[0].Name)
	assert.Equal(t, "p1", attrs[0].Type)
	assert.Equal(t, "tenant_id", attrs[1].Name)
	assert.Equal(t, "p2", attrs[1].Type)

	require.Len(t, counters, 1)
	assert.Equal(t, "my_counter", counters[0])
}

func TestParseDirectives(t *testing.T) {
	doc := &ast.CommentGroup{
		List: []*ast.Comment{
			{Text: "// Save stores the user."},
			{Text: "//middlegen:non-transactional"},
			{Text: "//middlegen:echo user, other"},
			{Text: "//middlegen:redact password token"},
			{Text: "//gen:redact ignored-other-prefix"},
		},
	}

	d := parseDirectives(doc, nil, "middlegen")
	assert.True(t, d.nonTransactional)
	assert.Equal(t, []string{"user", "other"}, d.echo)
	assert.False(t, d.echoNone)
	assert.Equal(t, map[string]bool{"password": true, "token": true}, d.redact)

	none := parseDirectives(&ast.CommentGroup{List: []*ast.Comment{{Text: "//middlegen:echo none"}}}, nil, "middlegen")
	assert.True(t, none.echoNone)
	assert.Empty(t, none.echo)
}

func TestSanitizeParamName(t *testing.T) {
	for _, reserved := range []string{"ok", "time", "uow", "slog", "err", "ctx", "r0", "r12"} {
		assert.Equal(t, reserved+"Arg", sanitizeParamName(reserved), reserved)
	}
	assert.Equal(t, "user", sanitizeParamName("user"))
	assert.Equal(t, "r", sanitizeParamName("r"))
}

func TestRewriteIdentifiers(t *testing.T) {
	m := Method{Params: []Field{
		{Name: "ctx", Label: "c", Type: "context.Context"},
		{Name: "tArg", Label: "t", Type: "*Thing"},
		{Name: "user", Label: "user", Type: "string"},
	}}

	assert.Equal(t, "tArg.ID", m.rewriteIdentifiers("t.ID"))
	assert.Equal(t, "x.t", m.rewriteIdentifiers("x.t"), "selector fields are not parameters")
	assert.Equal(t, "len(tArg.ID)+len(user)", m.rewriteIdentifiers("len(t.ID)+len(user)"))
	assert.Equal(t, "tenant", m.rewriteIdentifiers("tenant"), "prefix of a longer identifier is untouched")
}

func TestAssumedImportName(t *testing.T) {
	testCases := map[string]string{
		"context":                        "context",
		"github.com/jackc/pgx/v5":        "pgx",
		"gopkg.in/yaml.v3":               "yaml",
		"github.com/DATA-DOG/go-sqlmock": "sqlmock",
		"example.com/api/pb":             "pb",
	}
	for in, want := range testCases {
		assert.Equal(t, want, assumedImportName(in), in)
	}
}

func TestEchoStatement(t *testing.T) {
	thing := func(params ...Field) Method {
		return Method{Name: "M", Params: append([]Field{{Name: "ctx", Label: "ctx", Type: "context.Context"}}, params...)}
	}
	noDirectives := methodDirectives{redact: map[string]bool{}}

	t.Run("no results", func(t *testing.T) {
		stmt, err := thing().echoStatement(noDirectives, "I", "middlegen")
		require.NoError(t, err)
		assert.Equal(t, "return", stmt)
	})

	t.Run("unique entity parameter is echoed", func(t *testing.T) {
		m := thing(Field{Name: "u", Label: "u", Type: "*User"})
		m.Results = []Field{{Name: "r0", Type: "*User"}, {Name: "err", Type: "error"}}
		stmt, err := m.echoStatement(noDirectives, "I", "middlegen")
		require.NoError(t, err)
		assert.Equal(t, "return u, nil", stmt)
	})

	t.Run("basic types are never echoed", func(t *testing.T) {
		m := thing(Field{Name: "id", Label: "id", Type: "string"})
		m.Results = []Field{{Name: "r0", Type: "string"}, {Name: "r1", Type: "int"}, {Name: "err", Type: "error"}}
		stmt, err := m.echoStatement(noDirectives, "I", "middlegen")
		require.NoError(t, err)
		assert.Equal(t, `return "", 0, nil`, stmt)
	})

	t.Run("dereference is nil guarded", func(t *testing.T) {
		m := thing(Field{Name: "u", Label: "u", Type: "*User"})
		m.Results = []Field{{Name: "r0", Type: "User"}, {Name: "err", Type: "error"}}
		stmt, err := m.echoStatement(noDirectives, "I", "middlegen")
		require.NoError(t, err)
		assert.Contains(t, stmt, "var r0 User")
		assert.Contains(t, stmt, "if u != nil {")
		assert.Contains(t, stmt, "r0 = *u")
		assert.Contains(t, stmt, "return r0, nil")
	})

	t.Run("ambiguous candidates fall back to zero value", func(t *testing.T) {
		m := thing(Field{Name: "a", Label: "a", Type: "*User"}, Field{Name: "b", Label: "b", Type: "*User"})
		m.Results = []Field{{Name: "r0", Type: "*User"}, {Name: "err", Type: "error"}}
		stmt, err := m.echoStatement(noDirectives, "I", "middlegen")
		require.NoError(t, err)
		assert.Equal(t, "return nil, nil", stmt)
	})

	t.Run("explicit echo picks a candidate and may take a value address", func(t *testing.T) {
		m := thing(Field{Name: "a", Label: "a", Type: "User"}, Field{Name: "b", Label: "b", Type: "*User"})
		m.Results = []Field{{Name: "r0", Type: "*User"}, {Name: "err", Type: "error"}}
		stmt, err := m.echoStatement(methodDirectives{echo: []string{"a"}}, "I", "middlegen")
		require.NoError(t, err)
		assert.Equal(t, "return &a, nil", stmt)
	})

	t.Run("explicit echo none", func(t *testing.T) {
		m := thing(Field{Name: "u", Label: "u", Type: "*User"})
		m.Results = []Field{{Name: "r0", Type: "*User"}, {Name: "err", Type: "error"}}
		stmt, err := m.echoStatement(methodDirectives{echoNone: true}, "I", "middlegen")
		require.NoError(t, err)
		assert.Equal(t, "return nil, nil", stmt)
	})

	t.Run("explicit echo errors", func(t *testing.T) {
		m := thing(Field{Name: "u", Label: "u", Type: "*User"})
		m.Results = []Field{{Name: "r0", Type: "*Order"}, {Name: "err", Type: "error"}}

		_, err := m.echoStatement(methodDirectives{echo: []string{"missing"}}, "I", "middlegen")
		assert.ErrorContains(t, err, `unknown parameter "missing"`)

		_, err = m.echoStatement(methodDirectives{echo: []string{"u"}}, "I", "middlegen")
		assert.ErrorContains(t, err, "cannot be returned as result 0")

		_, err = m.echoStatement(methodDirectives{echo: []string{"u", "u"}}, "I", "middlegen")
		assert.ErrorContains(t, err, "names 2 parameters but the method has 1")
	})
}

func TestQualifyType(t *testing.T) {
	declared := map[string]bool{
		"User":      true,
		"CustomErr": true,
	}

	alias := "clientdb"

	testCases := []struct {
		input    string
		expected string
	}{
		{"User", "clientdb.User"},
		{"*User", "*clientdb.User"},
		{"[]User", "[]clientdb.User"},
		{"map[string]User", "map[string]clientdb.User"},
		{"string", "string"},
		{"context.Context", "context.Context"},
	}

	for _, tc := range testCases {
		assert.Equal(t, tc.expected, qualifyType(tc.input, declared, alias))
	}
}

func TestModuleHelpers(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "middlegen-test-*")
	require.NoError(t, err)
	defer os.RemoveAll(tempDir)

	goModContent := `module github.com/my-user/my-project

go 1.22
`
	err = os.WriteFile(filepath.Join(tempDir, "go.mod"), []byte(goModContent), 0640)
	require.NoError(t, err)

	nestedDir := filepath.Join(tempDir, "pkg", "sub")
	err = os.MkdirAll(nestedDir, 0750)
	require.NoError(t, err)

	// Test findModuleRoot
	root, err := findModuleRoot(nestedDir)
	assert.NoError(t, err)
	assert.Equal(t, tempDir, root)

	// Test getModuleName
	modName, err := getModuleName(tempDir)
	assert.NoError(t, err)
	assert.Equal(t, "github.com/my-user/my-project", modName)

	// Test detectLibraryModule
	libMod := detectLibraryModule(tempDir)
	assert.Equal(t, "github.com/pobochiigo/silo", libMod) // falls back or checks contents
}

func TestRunRejectsGenericInterfaces(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/g\n\ngo 1.26.0\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "g.go"), []byte("package g\n\ntype Store[T any] interface{ Get() T }\n"), 0o600))

	err := run(dir, options{TypeName: "Store", Kinds: []string{"logging"}})
	assert.ErrorContains(t, err, "generic interfaces")
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
