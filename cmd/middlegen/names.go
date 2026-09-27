package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode"
)

// reservedParamNames are identifiers declared or referenced by the generated
// middleware bodies: receivers, locals, and the packages the templates
// import. Interface parameters with these names are renamed so they cannot
// shadow or redeclare anything in the generated code.
var reservedParamNames = map[string]bool{
	// receivers and locals
	"l": true, "t": true, "m": true, "ok": true,
	"err": true, "span": true, "now": true, "ctx": true,
	"logger": true, "tracer": true, "recorder": true, "next": true, "manager": true,
	"uowInstance": true, "txCtx": true, "uowCtx": true, "innerErr": true,
	// packages referenced inside generated bodies
	"context": true, "time": true, "fmt": true, "slog": true, "otel": true,
	"codes": true, "trace": true, "attribute": true, "metric": true,
	"telemetry": true, "uow": true, "middleware": true,
}

// sanitizeParamName renames parameters that would collide with identifiers
// declared by the middleware templates (including the r0..rN result locals).
func sanitizeParamName(name string) string {
	if reservedParamNames[name] || (strings.HasPrefix(name, "r") && isDigit(name[1:])) {
		return name + "Arg"
	}
	return name
}

func isDigit(s string) bool {
	if len(s) == 0 {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// toSnakeCase converts CamelCase to snake_case, keeping acronyms together:
// GetByID -> get_by_id, HTTPClient -> http_client.
func toSnakeCase(str string) string {
	runes := []rune(str)
	var buf strings.Builder
	for i, r := range runes {
		if !unicode.IsUpper(r) {
			buf.WriteRune(r)
			continue
		}
		if i > 0 {
			prev := runes[i-1]
			nextLower := i+1 < len(runes) && unicode.IsLower(runes[i+1])
			if unicode.IsLower(prev) || unicode.IsDigit(prev) || (unicode.IsUpper(prev) && nextLower) {
				buf.WriteByte('_')
			}
		}
		buf.WriteRune(unicode.ToLower(r))
	}
	return buf.String()
}

// toCamelCase converts snake_case to lowerCamelCase.
func toCamelCase(str string) string {
	var buf strings.Builder
	for i, part := range strings.Split(str, "_") {
		if part == "" {
			continue
		}
		if i == 0 {
			buf.WriteString(strings.ToLower(part))
		} else {
			buf.WriteString(strings.ToUpper(part[:1]) + part[1:])
		}
	}
	return buf.String()
}

var basicTypes = map[string]bool{
	"string": true, "bool": true, "byte": true, "rune": true, "uintptr": true,
	"int": true, "int8": true, "int16": true, "int32": true, "int64": true,
	"uint": true, "uint8": true, "uint16": true, "uint32": true, "uint64": true,
	"float32": true, "float64": true, "complex64": true, "complex128": true,
	"error": true, "any": true, "interface{}": true,
}

// isBasicType reports whether typeStr names a predeclared type. Values of
// these types are never echoed automatically: a string parameter is rarely
// the string a write would return.
func isBasicType(typeStr string) bool {
	return basicTypes[strings.TrimPrefix(typeStr, "*")]
}

// getZeroValue returns a Go expression for the zero value of typeStr.
func getZeroValue(typeStr string) string {
	switch {
	case typeStr == "error", typeStr == "any", typeStr == "interface{}":
		return "nil"
	case strings.HasPrefix(typeStr, "*"), strings.HasPrefix(typeStr, "[]"),
		strings.HasPrefix(typeStr, "map["), strings.HasPrefix(typeStr, "chan "),
		strings.HasPrefix(typeStr, "func("):
		return "nil"
	case typeStr == "string":
		return `""`
	case typeStr == "bool":
		return "false"
	case typeStr == "float32", typeStr == "float64":
		return "0.0"
	case basicTypes[typeStr]:
		return "0"
	}
	// *new(T) yields the zero value of any type, including structs,
	// interfaces, and named types, where a T(0) conversion would not compile.
	return "*new(" + typeStr + ")"
}

func findModuleRoot(startDir string) (string, error) {
	dir := startDir
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("go.mod not found")
		}
		dir = parent
	}
}

func getModuleName(moduleRoot string) (string, error) {
	data, err := os.ReadFile(filepath.Join(moduleRoot, "go.mod"))
	if err != nil {
		return "", err
	}
	for line := range strings.SplitSeq(string(data), "\n") {
		line = strings.TrimSpace(line)
		if after, ok := strings.CutPrefix(line, "module "); ok {
			return strings.TrimSpace(after), nil
		}
	}
	return "", fmt.Errorf("module name not found in go.mod")
}

const defaultLibraryModule = "github.com/pobochiigo/silo"

// detectLibraryModule returns the module providing the middleware, telemetry
// and uow packages: the current module when generating inside silo itself,
// the published module otherwise.
func detectLibraryModule(moduleRoot string) string {
	if moduleName, err := getModuleName(moduleRoot); err == nil && moduleName == defaultLibraryModule {
		return moduleName
	}
	return defaultLibraryModule
}
