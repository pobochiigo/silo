package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"strings"
	"unicode"
)

// methodDirectives holds the parsed //<prefix>:... comments of one method.
type methodDirectives struct {
	nonTransactional bool
	inTx             bool
	echo             []string
	echoNone         bool
	redact           map[string]bool
	attrs            []Field
	counters         []string
}

// directivesFor reads the directives written on the declaration of fn,
// wherever that declaration lives: the loaded package, another package of
// the module, a dependency, or the standard library.
func (g *generator) directivesFor(fn *types.Func) methodDirectives {
	none := methodDirectives{redact: map[string]bool{}}

	pos := g.fset.Position(fn.Pos())
	if !pos.IsValid() || pos.Filename == "" {
		return none
	}
	file, fset := g.fileAST(pos.Filename)
	if file == nil {
		return none
	}

	var found *ast.Field
	ast.Inspect(file, func(n ast.Node) bool {
		if found != nil {
			return false
		}
		it, ok := n.(*ast.InterfaceType)
		if !ok || it.Methods == nil {
			return true
		}
		for _, field := range it.Methods.List {
			for _, ident := range field.Names {
				if ident.Name == fn.Name() && fset.Position(ident.Pos()).Line == pos.Line {
					found = field
					return false
				}
			}
		}
		return true
	})
	if found == nil {
		return none
	}
	return parseDirectives(found.Doc, found.Comment, g.opts.Prefix)
}

// fileAST returns the syntax of filename with comments: from the loaded
// package when it belongs to it, otherwise parsed on demand and cached.
func (g *generator) fileAST(filename string) (*ast.File, *token.FileSet) {
	for _, f := range g.pkg.Syntax {
		if g.fset.Position(f.Pos()).Filename == filename {
			return f, g.fset
		}
	}
	if f, ok := g.astCache[filename]; ok {
		return f, g.extFset
	}
	f, err := parser.ParseFile(g.extFset, filename, nil, parser.ParseComments)
	if err != nil {
		f = nil
	}
	g.astCache[filename] = f
	return f, g.extFset
}

// parseDirectives reads the directives attached to a method, from both its
// doc comment and its trailing line comment.
func parseDirectives(doc *ast.CommentGroup, line *ast.CommentGroup, prefix string) methodDirectives {
	d := methodDirectives{redact: map[string]bool{}}

	var comments []string
	for _, group := range []*ast.CommentGroup{doc, line} {
		if group == nil {
			continue
		}
		for _, c := range group.List {
			comments = append(comments, strings.TrimSpace(c.Text))
		}
	}

	marker := "//" + prefix + ":"
	for _, comment := range comments {
		body, ok := strings.CutPrefix(comment, marker)
		if !ok {
			continue
		}
		directive, arg, _ := strings.Cut(body, " ")
		arg = strings.TrimSpace(arg)

		switch directive {
		case "non-transactional":
			d.nonTransactional = true
		case "in-tx":
			d.inTx = true
		case "echo":
			if arg == "none" {
				d.echoNone = true
				continue
			}
			d.echo = append(d.echo, splitList(arg)...)
		case "redact":
			for _, name := range splitList(arg) {
				d.redact[name] = true
			}
		case "metric":
			if after, ok := strings.CutPrefix(arg, "attr:"); ok {
				if key, expr, ok := strings.Cut(after, "="); ok {
					d.attrs = append(d.attrs, Field{Name: strings.TrimSpace(key), Type: strings.TrimSpace(expr)})
				}
			} else if after, ok := strings.CutPrefix(arg, "counter:"); ok {
				d.counters = append(d.counters, strings.TrimSpace(after))
			}
		}
	}
	return d
}

// splitList splits a comma- or space-separated list of names.
func splitList(s string) []string {
	return strings.FieldsFunc(s, func(r rune) bool { return r == ',' || unicode.IsSpace(r) })
}
