package main

import (
	"go/ast"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestToSnakeCase(t *testing.T) {
	testCases := []struct {
		input    string
		expected string
	}{
		{"UserRepository", "user_repository"},
		{"UserService", "user_service"},
		{"GetByID", "get_by_i_d"},
		{"User", "user"},
	}

	for _, tc := range testCases {
		assert.Equal(t, tc.expected, toSnakeCase(tc.input))
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
