package main

import (
	"fmt"
	"go/types"
	"log"
	"regexp"
	"strings"
)

// Field is a parameter or result of a method as the templates see it.
type Field struct {
	Name string
	Type string
	// Label preserves the original parameter name for log keys and
	// directives when Name had to be renamed.
	Label string
	// Redact marks a parameter whose value must not appear in logs.
	Redact bool
	// IsContext marks the parameter the templates reference as ctx.
	IsContext bool
	// Variadic marks a trailing ...T parameter.
	Variadic bool
	// IsError marks a result of the predeclared error type.
	IsError bool
}

// Method is one decorated method with every string the templates need.
type Method struct {
	Name                string
	Params              []Field
	Results             []Field
	HasContext          bool
	HasError            bool
	NonTransactional    bool
	ParamsSignature     string
	ParamsNames         string
	ResultsSignature    string
	ResultsVars         string
	NonErrorResultsVars string
	SlogAttributes      string
	ParamsNamesWithTx   string
	ParamsNamesWithUow  string
	CustomAttributes    []Field  // Name is the attribute, Type the expression
	CustomCounters      []string // struct field names of the counters to increment
	// RepoDeferStmt is the body of the closure a deferred uow_repo method queues.
	RepoDeferStmt string
	// RepoEchoStmt is the statement a deferred uow_repo method executes to
	// return immediately: echoed parameters or zero values, nil-safe.
	RepoEchoStmt string

	counterMetrics []string
}

// buildMethod turns a type-checked method into template data.
func (g *generator) buildMethod(fn *types.Func, ifaceName string) (Method, error) {
	sig, ok := fn.Type().(*types.Signature)
	if !ok {
		return Method{}, fmt.Errorf("%s.%s is not a method", ifaceName, fn.Name())
	}
	d := g.directivesFor(fn)

	m := Method{
		Name:             fn.Name(),
		NonTransactional: d.nonTransactional,
		CustomAttributes: d.attrs,
		counterMetrics:   d.counters,
	}
	for _, metric := range d.counters {
		m.CustomCounters = append(m.CustomCounters, toCamelCase(metric)+"Counter")
	}

	// Render every type first so all package qualifiers are known before
	// parameter names are checked against them.
	params := sig.Params()
	paramTypes := make([]string, params.Len())
	for i := 0; i < params.Len(); i++ {
		t := params.At(i).Type()
		if hasInvalidType(t) {
			return Method{}, fmt.Errorf("%s.%s: parameter %d has an invalid type; fix the type errors reported above", ifaceName, fn.Name(), i)
		}
		if sig.Variadic() && i == params.Len()-1 {
			slice, ok := types.Unalias(t).(*types.Slice)
			if !ok {
				return Method{}, fmt.Errorf("%s.%s: variadic parameter is not a slice", ifaceName, fn.Name())
			}
			paramTypes[i] = "..." + g.typeString(slice.Elem())
		} else {
			paramTypes[i] = g.typeString(t)
		}
	}
	results := sig.Results()
	resultTypes := make([]string, results.Len())
	for i := 0; i < results.Len(); i++ {
		t := results.At(i).Type()
		if hasInvalidType(t) {
			return Method{}, fmt.Errorf("%s.%s: result %d has an invalid type; fix the type errors reported above", ifaceName, fn.Name(), i)
		}
		resultTypes[i] = g.typeString(t)
	}

	ctxNamed := false
	for i := 0; i < params.Len(); i++ {
		v := params.At(i)
		name := v.Name()
		if name == "" || name == "_" {
			name = fmt.Sprintf("p%d", i)
		}
		f := Field{
			Type:     paramTypes[i],
			Label:    name,
			Variadic: sig.Variadic() && i == params.Len()-1,
		}
		if !ctxNamed && isContextType(v.Type()) {
			// Templates reference the context parameter as "ctx",
			// regardless of how the interface names it.
			f.Name, f.Label, f.IsContext = "ctx", "ctx", true
			ctxNamed = true
			m.HasContext = true
		} else {
			f.Name = g.paramName(name)
		}
		f.Redact = d.redact[f.Label]
		m.Params = append(m.Params, f)
	}
	for name := range d.redact {
		if _, ok := m.param(name); !ok {
			return Method{}, fmt.Errorf("%s.%s: //%s:redact names unknown parameter %q", ifaceName, fn.Name(), g.opts.Prefix, name)
		}
	}

	// Result names from the interface declaration are deliberately ignored:
	// generated bodies declare their own locals (r0..rN, "err" for a
	// trailing error).
	for i := 0; i < results.Len(); i++ {
		m.Results = append(m.Results, Field{
			Name:    fmt.Sprintf("r%d", i),
			Type:    resultTypes[i],
			IsError: isErrorType(results.At(i).Type()),
		})
	}
	if n := len(m.Results); n > 0 && m.Results[n-1].IsError {
		m.Results[n-1].Name = "err"
		m.HasError = true
	}

	// Metric attribute expressions were written against the original
	// parameter names; rewrite any that were renamed.
	for i := range m.CustomAttributes {
		m.CustomAttributes[i].Type = m.rewriteIdentifiers(m.CustomAttributes[i].Type)
	}

	m.finalize()

	echo, err := m.echoStatement(d, ifaceName, g.opts.Prefix)
	if err != nil {
		return Method{}, err
	}
	m.RepoEchoStmt = echo
	return m, nil
}

// finalize derives the signature and call strings from Params and Results.
func (m *Method) finalize() {
	var paramsSig, paramsNames, withTx, withUow, slogAttrs []string
	for _, p := range m.Params {
		paramsSig = append(paramsSig, p.Name+" "+p.Type)
		call := p.Name
		if p.Variadic {
			call += "..." // variadic params must be re-spread when forwarding
		}
		paramsNames = append(paramsNames, call)
		if p.IsContext {
			withTx = append(withTx, "txCtx")
			withUow = append(withUow, "uowCtx")
			continue
		}
		withTx = append(withTx, call)
		withUow = append(withUow, call)
		if p.Redact {
			slogAttrs = append(slogAttrs, fmt.Sprintf("slog.String(%q, %q)", p.Label, "[REDACTED]"))
		} else {
			slogAttrs = append(slogAttrs, fmt.Sprintf("slog.Any(%q, %s)", p.Label, p.Name))
		}
	}

	var resultsSig, resultsVars, nonErrorVars []string
	for _, r := range m.Results {
		resultsSig = append(resultsSig, r.Type)
		resultsVars = append(resultsVars, r.Name)
		if !r.IsError {
			nonErrorVars = append(nonErrorVars, r.Name)
		}
	}

	m.ParamsSignature = strings.Join(paramsSig, ", ")
	m.ParamsNames = strings.Join(paramsNames, ", ")
	m.ParamsNamesWithTx = strings.Join(withTx, ", ")
	m.ParamsNamesWithUow = strings.Join(withUow, ", ")
	m.SlogAttributes = strings.Join(slogAttrs, ", ")
	m.ResultsSignature = strings.Join(resultsSig, ", ")
	if len(m.Results) > 1 {
		m.ResultsSignature = "(" + m.ResultsSignature + ")"
	}
	m.ResultsVars = strings.Join(resultsVars, ", ")
	m.NonErrorResultsVars = strings.Join(nonErrorVars, ", ")

	switch {
	case len(m.Results) == 1 && m.HasError:
		m.RepoDeferStmt = fmt.Sprintf("return m.next.%s(%s)", m.Name, m.ParamsNamesWithTx)
	case len(m.Results) > 1 && m.HasError:
		blanks := strings.Repeat("_, ", len(m.Results)-1)
		m.RepoDeferStmt = fmt.Sprintf("%serr := m.next.%s(%s)\n\t\t\treturn err", blanks, m.Name, m.ParamsNamesWithTx)
	default:
		m.RepoDeferStmt = fmt.Sprintf("m.next.%s(%s)\n\t\t\treturn nil", m.Name, m.ParamsNamesWithTx)
	}
}

// param looks a parameter up by its original (label) or generated name.
func (m Method) param(name string) (Field, bool) {
	for _, p := range m.Params {
		if p.Label == name || p.Name == name {
			return p, true
		}
	}
	return Field{}, false
}

// rewriteIdentifiers replaces original parameter names inside a user-written
// expression with the generated names, so metric attribute expressions keep
// working after a parameter was renamed (or the context parameter became ctx).
func (m Method) rewriteIdentifiers(expr string) string {
	for _, p := range m.Params {
		if p.Label == p.Name {
			continue
		}
		// An identifier boundary that is not a selector field (x.label).
		re := regexp.MustCompile(`(^|[^.\w])` + regexp.QuoteMeta(p.Label) + `\b`)
		expr = re.ReplaceAllString(expr, "${1}"+p.Name)
	}
	return expr
}

// echoStatement builds the return statement a deferred uow_repo method
// executes immediately. Non-error results are, in order of preference:
//
//  1. the parameters named by an //<prefix>:echo directive ("none" disables echoing);
//  2. the single parameter whose type matches the result (also *T for T and
//     T for *T), when that type is not a basic type;
//  3. the zero value.
//
// Echoing hands the caller back the object it passed in, which is the source
// of truth for a write that has not happened yet. A dereferenced pointer
// parameter is guarded against nil.
func (m Method) echoStatement(d methodDirectives, ifaceName, prefix string) (string, error) {
	if len(m.Results) == 0 {
		return "return", nil
	}

	nonError := make([]int, 0, len(m.Results))
	for i, r := range m.Results {
		if !r.IsError {
			nonError = append(nonError, i)
		}
	}
	if !d.echoNone && len(d.echo) > len(nonError) {
		return "", fmt.Errorf("%s.%s: //%s:echo names %d parameters but the method has %d non-error results",
			ifaceName, m.Name, prefix, len(d.echo), len(nonError))
	}

	exprs := make([]string, len(m.Results))
	var preamble []string
	for i, r := range m.Results {
		exprs[i] = getZeroValue(r.Type)
	}

	for pos, idx := range nonError {
		r := m.Results[idx]
		var src Field
		var found bool

		switch {
		case d.echoNone:
			continue
		case pos < len(d.echo):
			p, ok := m.param(d.echo[pos])
			if !ok {
				return "", fmt.Errorf("%s.%s: //%s:echo names unknown parameter %q", ifaceName, m.Name, prefix, d.echo[pos])
			}
			if p.IsContext || p.Variadic || !echoCompatible(p.Type, r.Type) {
				return "", fmt.Errorf("%s.%s: //%s:echo parameter %q (%s) cannot be returned as result %d (%s)",
					ifaceName, m.Name, prefix, d.echo[pos], p.Type, pos, r.Type)
			}
			src, found = p, true
		case isBasicType(r.Type):
			continue
		default:
			var candidates []Field
			for _, p := range m.Params {
				if !p.IsContext && !p.Variadic && echoCompatible(p.Type, r.Type) {
					candidates = append(candidates, p)
				}
			}
			switch len(candidates) {
			case 1:
				src, found = candidates[0], true
			case 0:
			default:
				log.Printf("Warning: %s.%s: %d parameters match result %d (%s); returning its zero value when deferred. Use //%s:echo <param> to choose one",
					ifaceName, m.Name, len(candidates), pos, r.Type, prefix)
			}
		}

		if !found {
			continue
		}
		switch {
		case src.Type == r.Type:
			exprs[idx] = src.Name
		case strings.HasPrefix(r.Type, "*") && r.Type[1:] == src.Type:
			exprs[idx] = "&" + src.Name
		default: // r.Type == src.Type[1:]: dereference, guarded against nil
			preamble = append(preamble,
				fmt.Sprintf("var %s %s", r.Name, r.Type),
				fmt.Sprintf("if %s != nil {\n\t\t\t%s = *%s\n\t\t}", src.Name, r.Name, src.Name),
			)
			exprs[idx] = r.Name
		}
	}

	stmt := "return " + strings.Join(exprs, ", ")
	if len(preamble) > 0 {
		stmt = strings.Join(preamble, "\n\t\t") + "\n\t\t" + stmt
	}
	return stmt, nil
}

// echoCompatible reports whether a parameter of type paramType can be
// returned as a result of type resultType.
func echoCompatible(paramType, resultType string) bool {
	if paramType == resultType {
		return true
	}
	if strings.HasPrefix(resultType, "*") && resultType[1:] == paramType {
		return true
	}
	if strings.HasPrefix(paramType, "*") && paramType[1:] == resultType {
		return true
	}
	return false
}
