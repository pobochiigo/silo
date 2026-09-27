package main

import (
	"fmt"
	"go/printer"
	"go/token"
	"go/types"
	"io"
	"path"
	"sort"
)

// importEntry is one package the generated code may reference.
type importEntry struct {
	path    string
	name    string // qualifier used in the generated code
	pkgName string // the package's declared name, once seen
	used    bool   // referenced by a signature (templates track their own imports)
}

// importSet assigns a unique qualifier to every package referenced from the
// generated code and renders the import lines a template does not declare
// itself. Names the templates use for their own imports and locals are
// reserved so a user package can never shadow them.
type importSet struct {
	destPath string
	entries  map[string]*importEntry
	taken    map[string]bool
}

// newImportSet creates an importSet for code generated into destPath. fixed
// maps the import paths the templates declare themselves to the qualifiers
// they use for them.
func newImportSet(destPath string, fixed map[string]string) *importSet {
	s := &importSet{
		destPath: destPath,
		entries:  map[string]*importEntry{},
		taken:    map[string]bool{},
	}
	for name := range reservedParamNames {
		s.taken[name] = true
	}
	for p, name := range fixed {
		s.entries[p] = &importEntry{path: p, name: name}
		s.taken[name] = true
	}
	return s
}

// fixedImportNames lists the imports the templates declare with the
// qualifiers they use.
func fixedImportNames(libModule, mwImport, mwQualifier string) map[string]string {
	return map[string]string{
		"context":                            "context",
		"fmt":                                "fmt",
		"log/slog":                           "slog",
		"time":                               "time",
		"go.opentelemetry.io/otel":           "otel",
		"go.opentelemetry.io/otel/attribute": "attribute",
		"go.opentelemetry.io/otel/codes":     "codes",
		"go.opentelemetry.io/otel/metric":    "metric",
		"go.opentelemetry.io/otel/trace":     "trace",
		libModule + "/telemetry":             "telemetry",
		libModule + "/uow":                   "uow",
		mwImport:                             mwQualifier,
	}
}

// qualifier implements types.Qualifier: it returns the qualifier for p, or
// "" for the destination package, allocating a conflict-free name on first
// use.
func (s *importSet) qualifier(p *types.Package) string {
	if p == nil || p.Path() == s.destPath {
		return ""
	}
	e, ok := s.entries[p.Path()]
	if !ok {
		name := p.Name()
		candidate := name
		for i := 2; s.taken[candidate]; i++ {
			candidate = fmt.Sprintf("%s%d", name, i)
		}
		e = &importEntry{path: p.Path(), name: candidate}
		s.entries[p.Path()] = e
		s.taken[candidate] = true
	}
	e.pkgName = p.Name()
	e.used = true
	return e.name
}

// qualifiedName returns name qualified for use from the destination package.
func (s *importSet) qualifiedName(p *types.Package, name string) string {
	if q := s.qualifier(p); q != "" {
		return q + "." + name
	}
	return name
}

// isName reports whether name is the qualifier of a package a signature uses.
func (s *importSet) isName(name string) bool {
	for _, e := range s.entries {
		if e.used && e.name == name {
			return true
		}
	}
	return false
}

// specs renders the import lines for the used packages whose path is not in
// fixed (the template writes those itself), sorted by path.
func (s *importSet) specs(fixed map[string]bool) []string {
	var paths []string
	for p, e := range s.entries {
		if e.used && !fixed[p] {
			paths = append(paths, p)
		}
	}
	sort.Strings(paths)

	specs := make([]string, 0, len(paths))
	for _, p := range paths {
		e := s.entries[p]
		if e.pkgName != "" && (e.name != e.pkgName || path.Base(e.path) != e.pkgName) {
			specs = append(specs, fmt.Sprintf("%s %q", e.name, e.path))
		} else {
			specs = append(specs, fmt.Sprintf("%q", e.path))
		}
	}
	return specs
}

func printNode(w io.Writer, fset *token.FileSet, node any) error {
	return printer.Fprint(w, fset, node)
}
