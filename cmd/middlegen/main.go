// Command middlegen parses a Go interface and generates decorator
// middlewares for it: logging, tracing, metrics, and Unit of Work
// boundaries for repositories and services.
//
// Directives are comments on interface methods, prefixed by -prefix
// (default "middlegen"):
//
//	//middlegen:non-transactional          run the repository method immediately, never defer it
//	//middlegen:echo <param>[, <param>...]  return these parameters, in order, as the deferred
//	                                        method's non-error results ("none" disables echoing)
//	//middlegen:redact <param>[, <param>...] log these parameters as "[REDACTED]"
//	//middlegen:metric attr:<name>=<expr>   add a metric attribute computed from an expression
//	//middlegen:metric counter:<name>       increment a custom counter on every call
package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/printer"
	"go/token"
	"log"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"text/template"
	"unicode"
)

type Field struct {
	Name string
	Type string
	// Label preserves the original parameter name for log keys when Name
	// had to be renamed to avoid colliding with template-declared identifiers.
	Label string
	// Redact marks a parameter whose value must not appear in logs.
	Redact bool
}

type Method struct {
	Name                string
	Params              []Field
	Results             []Field
	HasContext          bool
	HasError            bool
	ParamsSignature     string
	ParamsNames         string
	ResultsSignature    string
	ResultsVars         string
	NonErrorResultsVars string
	LogPlaceholders     string
	LogValues           string
	CustomAttributes    []Field
	CustomCounters      []string
	SlogAttributes      string
	ParamsNamesWithTx   string
	ParamsNamesWithUow  string
	NonTransactional    bool
	RepoDeferStmt       string
	// RepoEchoStmt is the statement a deferred uow_repo method executes to
	// return immediately: echoed parameters or zero values, nil-safe.
	RepoEchoStmt string
}

type CounterMeta struct {
	FieldName  string
	MetricName string
}

// importSpec is an import of the interface's file that generated code may need.
type importSpec struct {
	Path string // import path
	Name string // qualifier used in source (explicit alias or assumed name)
	Spec string // rendered import line, e.g. `pb "example.com/api/pb"`
}

type InterfaceMeta struct {
	PackageName                 string
	InterfaceName               string
	InterfaceNameWithoutPackage string
	InterfaceNameLower          string
	ServiceName                 string
	Imports                     []string
	Methods                     []Method
	CustomCounters              []CounterMeta
	HasCustomAttributes         bool
	HasErrorlessTxMethods       bool
	MiddlewareImport            string
	MiddlewareType              string
	LibraryModule               string

	usedImports []importSpec
}

// options are the generator inputs, normally taken from the command line.
type options struct {
	TypeName         string
	Kinds            []string
	Service          string
	Dir              string
	Prefix           string
	MiddlewareImport string
	MiddlewareType   string
	LibraryModule    string
	// OutDir receives the generated files; empty means the working directory.
	OutDir string
}

var (
	typeFlag             = flag.String("type", "", "The target interface name (required)")
	kindsFlag            = flag.String("kinds", "logging,tracing,metrics", "Comma-separated middleware kinds to generate (logging,tracing,metrics,uow_repo,uow_service)")
	serviceFlag          = flag.String("service", "", "Service tracing prefix name (optional)")
	dirFlag              = flag.String("dir", "", "Interface directory relative to module root (optional, e.g. client/puzzle)")
	prefixFlag           = flag.String("prefix", "middlegen", "Prefix namespace for comment directives (default: middlegen)")
	middlewareImportFlag = flag.String("middleware-import", "", "Import path for the generic Middleware type definition (defaults to <library-module>/middleware)")
	middlewareTypeFlag   = flag.String("middleware-type", "middleware.Middleware", "Fully-qualified type name for generic Middleware type")
	libraryModuleFlag    = flag.String("library-module", "", "Module path providing the middleware/telemetry/uow packages (defaults to autodetection)")
)

// reservedParamNames are identifiers declared or referenced by the generated
// middleware bodies: receivers, locals, and the packages the templates
// import. Interface parameters with these names are renamed so they cannot
// shadow or redeclare anything in the generated code.
var reservedParamNames = map[string]bool{
	// receivers and locals
	"l": true, "t": true, "m": true, "ok": true,
	"err": true, "span": true, "now": true, "ctx": true,
	"logger": true, "tracer": true, "recorder": true, "next": true,
	"uowInstance": true, "txCtx": true, "uowCtx": true, "innerErr": true,
	// packages referenced inside generated bodies
	"context": true, "time": true, "fmt": true, "slog": true, "otel": true,
	"codes": true, "trace": true, "attribute": true, "metric": true,
	"telemetry": true, "uow": true, "middleware": true,
}

// sanitizeParamName renames parameters that would collide with identifiers
// declared by the middleware templates.
func sanitizeParamName(name string) string {
	if reservedParamNames[name] || (strings.HasPrefix(name, "r") && isDigit(name[1:])) {
		return name + "Arg"
	}
	return name
}

func main() {
	flag.Parse()

	cwd, err := os.Getwd()
	if err != nil {
		log.Fatalf("Failed to get working directory: %v", err)
	}

	opts := options{
		TypeName:         *typeFlag,
		Kinds:            strings.Split(*kindsFlag, ","),
		Service:          *serviceFlag,
		Dir:              *dirFlag,
		Prefix:           *prefixFlag,
		MiddlewareImport: *middlewareImportFlag,
		MiddlewareType:   *middlewareTypeFlag,
		LibraryModule:    *libraryModuleFlag,
	}

	if err := run(cwd, opts); err != nil {
		log.Fatalf("Error: %v", err)
	}
}

// run generates the requested middlewares for the interface described by o,
// resolving paths relative to cwd.
func run(cwd string, o options) error {
	if o.TypeName == "" {
		return errors.New("-type flag is required")
	}
	if o.Prefix == "" {
		o.Prefix = "middlegen"
	}
	if o.MiddlewareType == "" {
		o.MiddlewareType = "middleware.Middleware"
	}

	moduleRoot, err := findModuleRoot(cwd)
	if err != nil {
		return fmt.Errorf("failed to find module root: %w", err)
	}

	targetDir := cwd
	if o.Dir != "" {
		targetDir = filepath.Join(moduleRoot, o.Dir)
	}
	outDir := o.OutDir
	if outDir == "" {
		outDir = cwd
	}

	filter := func(info os.FileInfo) bool {
		return !strings.HasSuffix(info.Name(), ".gen.go")
	}

	destPackageName := ""
	destFset := token.NewFileSet()
	//lint:ignore SA1019 middlegen reads a single directory and ignores build tags on purpose
	destPkgs, err := parser.ParseDir(destFset, cwd, filter, parser.ParseComments)
	if err == nil {
		for name := range destPkgs {
			// ParseDir also returns the external test package ("foo_test");
			// never use it as the destination package clause.
			if strings.HasSuffix(name, "_test") {
				continue
			}
			destPackageName = name
			break
		}
	}
	if destPackageName == "" {
		destPackageName = filepath.Base(cwd)
	}

	fset := token.NewFileSet()
	//lint:ignore SA1019 middlegen reads a single directory and ignores build tags on purpose
	pkgs, err := parser.ParseDir(fset, targetDir, filter, parser.ParseComments)
	if err != nil {
		return fmt.Errorf("failed to parse target directory %s: %w", targetDir, err)
	}

	var targetInterface *ast.TypeSpec
	var packageName string
	var targetFile *ast.File

	for name, pkg := range pkgs {
		if strings.HasSuffix(name, "_test") {
			continue
		}
		packageName = name
		for _, file := range pkg.Files {
			// Find interface
			foundInterface := false
			ast.Inspect(file, func(n ast.Node) bool {
				ts, ok := n.(*ast.TypeSpec)
				if !ok {
					return true
				}
				if ts.Name.Name == o.TypeName {
					if _, isInterface := ts.Type.(*ast.InterfaceType); isInterface {
						targetInterface = ts
						foundInterface = true
						return false
					}
				}
				return true
			})
			if foundInterface {
				targetFile = file
				break
			}
		}
		if targetInterface != nil {
			break
		}
	}

	if targetInterface == nil {
		return fmt.Errorf("interface %s not found in package %s", o.TypeName, packageName)
	}
	if targetInterface.TypeParams != nil && len(targetInterface.TypeParams.List) > 0 {
		return fmt.Errorf("interface %s has type parameters; middlegen does not support generic interfaces", o.TypeName)
	}

	isCrossPackage := targetDir != cwd
	alias := "client" + packageName
	declaredTypes := make(map[string]bool)

	if isCrossPackage {
		for _, pkg := range pkgs {
			for _, file := range pkg.Files {
				ast.Inspect(file, func(n ast.Node) bool {
					ts, ok := n.(*ast.TypeSpec)
					if ok {
						declaredTypes[ts.Name.Name] = true
					}
					return true
				})
			}
		}
	}

	parts := strings.Split(o.TypeName, ".")
	rawInterfaceName := parts[len(parts)-1]

	// Detect library module path
	libModule := o.LibraryModule
	if libModule == "" {
		libModule = detectLibraryModule(moduleRoot)
	}

	mwImport := o.MiddlewareImport
	if mwImport == "" {
		mwImport = libModule + "/middleware"
	}

	interfaceType := targetInterface.Type.(*ast.InterfaceType)
	meta := &InterfaceMeta{
		PackageName:                 destPackageName,
		InterfaceName:               o.TypeName,
		InterfaceNameWithoutPackage: rawInterfaceName,
		InterfaceNameLower:          strings.ToLower(rawInterfaceName[0:1]) + rawInterfaceName[1:],
		ServiceName:                 o.Service,
		MiddlewareImport:            mwImport,
		MiddlewareType:              o.MiddlewareType,
		LibraryModule:               libModule,
	}

	if isCrossPackage {
		meta.InterfaceName = alias + "." + o.TypeName
	}

	if meta.ServiceName == "" {
		meta.ServiceName = strings.ToLower(packageName)
	}

	// usedQualifiers collects the package qualifiers (pkg in pkg.Type) that
	// appear in method signatures, so only the imports they need are copied.
	usedQualifiers := make(map[string]bool)

	for _, methodField := range interfaceType.Methods.List {
		if len(methodField.Names) == 0 {
			// Embedded interface (e.g. io.Closer): its methods are not
			// visible from the AST of this file, so no middleware is
			// generated for them. The generated wrappers embed the wrapped
			// implementation, so these methods are forwarded undecorated.
			log.Printf("Warning: methods of embedded interface %s in %s are forwarded without middleware; declare them explicitly to decorate them",
				getASTNodeString(fset, methodField.Type), o.TypeName)
			continue
		}
		methodName := methodField.Names[0].Name
		funcType, ok := methodField.Type.(*ast.FuncType)
		if !ok {
			continue
		}

		method := Method{
			Name: methodName,
		}

		directives := parseDirectives(methodField.Doc, methodField.Comment, o.Prefix)
		method.CustomAttributes = directives.attrs
		method.NonTransactional = directives.nonTransactional

		var counterFields []string
		for _, c := range directives.counters {
			fieldName := toCamelCase(c) + "Counter"
			counterFields = append(counterFields, fieldName)

			// Store unique counter in meta
			found := false
			for _, mCounter := range meta.CustomCounters {
				if mCounter.MetricName == c {
					found = true
					break
				}
			}
			if !found {
				meta.CustomCounters = append(meta.CustomCounters, CounterMeta{
					FieldName:  fieldName,
					MetricName: c,
				})
			}
		}
		method.CustomCounters = counterFields

		// Parse Parameters
		if funcType.Params != nil {
			paramIdx := 0
			ctxNamed := false
			for _, p := range funcType.Params.List {
				collectQualifiers(p.Type, usedQualifiers)
				typeStr := getASTNodeString(fset, p.Type)
				if isCrossPackage {
					typeStr = qualifyType(typeStr, declaredTypes, alias)
				}
				var names []string
				if len(p.Names) == 0 {
					names = []string{fmt.Sprintf("p%d", paramIdx)}
				} else {
					for _, nameIdent := range p.Names {
						names = append(names, nameIdent.Name)
					}
				}
				for _, name := range names {
					label := name
					// Templates reference the context parameter as "ctx",
					// regardless of how the interface names it.
					if typeStr == "context.Context" && !ctxNamed {
						name, label = "ctx", "ctx"
						ctxNamed = true
					} else {
						name = sanitizeParamName(name)
					}
					method.Params = append(method.Params, Field{
						Name:   name,
						Type:   typeStr,
						Label:  label,
						Redact: directives.redact[label],
					})
					paramIdx++
				}
			}
		}
		for name := range directives.redact {
			if _, ok := method.param(name); !ok {
				return fmt.Errorf("%s.%s: //%s:redact names unknown parameter %q", o.TypeName, methodName, o.Prefix, name)
			}
		}

		// Parse Results. Result names from the interface declaration are
		// deliberately ignored: generated bodies declare their own locals
		// (r0..rN, plus "err" for a trailing error).
		if funcType.Results != nil {
			resultIdx := 0
			for _, r := range funcType.Results.List {
				collectQualifiers(r.Type, usedQualifiers)
				typeStr := getASTNodeString(fset, r.Type)
				if isCrossPackage {
					typeStr = qualifyType(typeStr, declaredTypes, alias)
				}
				count := len(r.Names)
				if count == 0 {
					count = 1
				}
				for i := 0; i < count; i++ {
					method.Results = append(method.Results, Field{Name: fmt.Sprintf("r%d", resultIdx), Type: typeStr})
					resultIdx++
				}
			}
			if n := len(method.Results); n > 0 && method.Results[n-1].Type == "error" {
				method.Results[n-1].Name = "err"
			}
		}

		// Analyse parameters & returns
		if len(method.Params) > 0 && method.Params[0].Type == "context.Context" {
			method.HasContext = true
		}
		if len(method.Results) > 0 && method.Results[len(method.Results)-1].Type == "error" {
			method.HasError = true
		}
		if method.HasContext && !method.HasError {
			meta.HasErrorlessTxMethods = true
		}

		// Metric attribute expressions were written against the original
		// parameter names; rewrite any that were renamed.
		for i := range method.CustomAttributes {
			method.CustomAttributes[i].Type = method.rewriteIdentifiers(method.CustomAttributes[i].Type)
		}

		// Construct signatures & values
		var paramsSig []string
		var paramsNames []string
		var logPlaceholders []string
		var logValues []string
		var slogAttrs []string

		for _, p := range method.Params {
			paramsSig = append(paramsSig, fmt.Sprintf("%s %s", p.Name, p.Type))
			callName := p.Name
			if strings.HasPrefix(p.Type, "...") {
				callName += "..." // variadic params must be re-spread when forwarding
			}
			paramsNames = append(paramsNames, callName)
			if p.Type != "context.Context" {
				logPlaceholders = append(logPlaceholders, fmt.Sprintf("%s=%%v", p.Label))
				logValues = append(logValues, p.Name)
				if p.Redact {
					slogAttrs = append(slogAttrs, fmt.Sprintf("slog.String(%q, %q)", p.Label, "[REDACTED]"))
				} else {
					slogAttrs = append(slogAttrs, fmt.Sprintf("slog.Any(%q, %s)", p.Label, p.Name))
				}
			}
		}

		var resultsSig []string
		var resultsVars []string
		var nonErrorResultsVars []string
		for _, r := range method.Results {
			resultsSig = append(resultsSig, r.Type)
			resultsVars = append(resultsVars, r.Name)
			if r.Type != "error" {
				nonErrorResultsVars = append(nonErrorResultsVars, r.Name)
			}
		}

		method.ParamsSignature = strings.Join(paramsSig, ", ")
		method.ParamsNames = strings.Join(paramsNames, ", ")
		method.ResultsSignature = strings.Join(resultsSig, ", ")
		if len(method.Results) > 1 {
			method.ResultsSignature = "(" + method.ResultsSignature + ")"
		}
		method.ResultsVars = strings.Join(resultsVars, ", ")
		method.NonErrorResultsVars = strings.Join(nonErrorResultsVars, ", ")
		method.LogPlaceholders = strings.Join(logPlaceholders, " ")
		method.LogValues = strings.Join(logValues, ", ")
		method.SlogAttributes = strings.Join(slogAttrs, ", ")

		var paramsNamesWithTx []string
		var paramsNamesWithUow []string
		for _, p := range method.Params {
			if p.Type == "context.Context" {
				paramsNamesWithTx = append(paramsNamesWithTx, "txCtx")
				paramsNamesWithUow = append(paramsNamesWithUow, "uowCtx")
			} else {
				callName := p.Name
				if strings.HasPrefix(p.Type, "...") {
					callName += "..."
				}
				paramsNamesWithTx = append(paramsNamesWithTx, callName)
				paramsNamesWithUow = append(paramsNamesWithUow, callName)
			}
		}
		method.ParamsNamesWithTx = strings.Join(paramsNamesWithTx, ", ")
		method.ParamsNamesWithUow = strings.Join(paramsNamesWithUow, ", ")

		echoStmt, err := method.echoStatement(directives, o.TypeName, o.Prefix)
		if err != nil {
			return err
		}
		method.RepoEchoStmt = echoStmt

		if len(method.Results) == 0 {
			method.RepoDeferStmt = fmt.Sprintf("m.next.%s(%s)\n\t\t\treturn nil", method.Name, method.ParamsNamesWithTx)
		} else if len(method.Results) == 1 && method.HasError {
			method.RepoDeferStmt = fmt.Sprintf("return m.next.%s(%s)", method.Name, method.ParamsNamesWithTx)
		} else if len(method.Results) > 1 && method.HasError {
			var underscores []string
			for i := 0; i < len(method.Results)-1; i++ {
				underscores = append(underscores, "_")
			}
			underscoreStr := strings.Join(underscores, ", ")
			method.RepoDeferStmt = fmt.Sprintf("%s, err := m.next.%s(%s)\n\t\t\treturn err", underscoreStr, method.Name, method.ParamsNamesWithTx)
		} else {
			method.RepoDeferStmt = fmt.Sprintf("m.next.%s(%s)\n\t\t\treturn nil", method.Name, method.ParamsNamesWithTx)
		}

		meta.Methods = append(meta.Methods, method)
	}

	hasCustomAttrs := false
	for _, method := range meta.Methods {
		if len(method.CustomAttributes) > 0 {
			hasCustomAttrs = true
			break
		}
	}
	meta.HasCustomAttributes = hasCustomAttrs

	// Copy the imports of the interface's file that its signatures use.
	for _, imp := range targetFile.Imports {
		if imp.Name != nil && (imp.Name.Name == "_" || imp.Name.Name == ".") {
			continue
		}
		impPath := strings.Trim(imp.Path.Value, `"`)
		name := getImportName(imp)
		if !usedQualifiers[name] && !usedQualifiers[path.Base(impPath)] {
			continue
		}
		var buf bytes.Buffer
		if err := printer.Fprint(&buf, fset, imp); err != nil {
			return fmt.Errorf("failed to render import %s: %w", impPath, err)
		}
		meta.usedImports = append(meta.usedImports, importSpec{Path: impPath, Name: name, Spec: buf.String()})
	}

	if isCrossPackage {
		relDir, err := filepath.Rel(moduleRoot, targetDir)
		if err == nil {
			moduleName, err := getModuleName(moduleRoot)
			if err == nil {
				interfaceImportPath := moduleName + "/" + filepath.ToSlash(relDir)
				meta.usedImports = append(meta.usedImports, importSpec{
					Path: interfaceImportPath,
					Name: alias,
					Spec: fmt.Sprintf(`%s "%s"`, alias, interfaceImportPath),
				})
			}
		}
	}

	for _, method := range meta.Methods {
		if method.HasContext && !method.HasError {
			for _, kind := range o.Kinds {
				if strings.TrimSpace(kind) == "uow_service" {
					log.Printf("Warning: %s.%s returns no error; Unit of Work failures in the generated uow_service middleware are logged via slog, not returned",
						o.TypeName, method.Name)
				}
			}
		}
	}

	prefix := toSnakeCase(o.TypeName)
	for _, kind := range o.Kinds {
		kind = strings.TrimSpace(kind)
		var filename string
		switch kind {
		case "logging", "tracing", "metrics":
			filename = fmt.Sprintf("%s_%s_middleware.gen.go", prefix, kind)
		case "uow_repo", "uow_service":
			filename = fmt.Sprintf("%s_uow_middleware.gen.go", prefix)
		default:
			return fmt.Errorf("unknown middleware kind: %s", kind)
		}
		if err := generateMiddleware(meta, kind, filepath.Join(outDir, filename)); err != nil {
			return err
		}
	}
	return nil
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
		if r.Type != "error" {
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
			if p.Type == "context.Context" || !echoCompatible(p.Type, r.Type) {
				return "", fmt.Errorf("%s.%s: //%s:echo parameter %q (%s) cannot be returned as result %d (%s)",
					ifaceName, m.Name, prefix, d.echo[pos], p.Type, pos, r.Type)
			}
			src, found = p, true
		case isBasicType(r.Type):
			continue
		default:
			var candidates []Field
			for _, p := range m.Params {
				if p.Type != "context.Context" && echoCompatible(p.Type, r.Type) {
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

// collectQualifiers records the package qualifiers used in a type expression.
func collectQualifiers(expr ast.Expr, used map[string]bool) {
	ast.Inspect(expr, func(n ast.Node) bool {
		if sel, ok := n.(*ast.SelectorExpr); ok {
			if id, ok := sel.X.(*ast.Ident); ok {
				used[id.Name] = true
			}
		}
		return true
	})
}

// importsExcluding renders the interface's used imports that a template does
// not already import itself.
func (meta *InterfaceMeta) importsExcluding(fixed map[string]bool) []string {
	var out []string
	for _, imp := range meta.usedImports {
		if fixed[imp.Path] {
			continue
		}
		out = append(out, imp.Spec)
	}
	return out
}

func getASTNodeString(fset *token.FileSet, node ast.Node) string {
	var buf bytes.Buffer
	if err := printer.Fprint(&buf, fset, node); err != nil {
		return ""
	}
	return buf.String()
}

// templateFor returns the template for kind and the import paths that
// template always declares itself.
func templateFor(kind string, meta *InterfaceMeta) (string, map[string]bool) {
	fixed := map[string]bool{"context": true, meta.MiddlewareImport: true}
	switch kind {
	case "logging":
		fixed["log/slog"] = true
		return loggingTemplate, fixed
	case "tracing":
		fixed["go.opentelemetry.io/otel"] = true
		fixed["go.opentelemetry.io/otel/codes"] = true
		fixed["go.opentelemetry.io/otel/trace"] = true
		return tracingTemplate, fixed
	case "metrics":
		fixed["time"] = true
		fixed["go.opentelemetry.io/otel"] = true
		fixed[meta.LibraryModule+"/telemetry"] = true
		if meta.HasCustomAttributes {
			fixed["fmt"] = true
			fixed["go.opentelemetry.io/otel/attribute"] = true
		}
		if len(meta.CustomCounters) > 0 {
			fixed["go.opentelemetry.io/otel/metric"] = true
		}
		return metricsTemplate, fixed
	case "uow_repo":
		fixed[meta.LibraryModule+"/uow"] = true
		return uowRepoTemplate, fixed
	case "uow_service":
		fixed[meta.LibraryModule+"/uow"] = true
		if meta.HasErrorlessTxMethods {
			fixed["log/slog"] = true
		}
		return uowServiceTemplate, fixed
	}
	return "", fixed
}

func generateMiddleware(meta *InterfaceMeta, kind string, filename string) error {
	tempStr, fixed := templateFor(kind, meta)
	if tempStr == "" {
		return fmt.Errorf("unknown middleware kind: %s", kind)
	}
	meta.Imports = meta.importsExcluding(fixed)

	tmpl, err := template.New("middleware").Funcs(template.FuncMap{
		"toSnakeCase": toSnakeCase,
	}).Parse(tempStr)
	if err != nil {
		return fmt.Errorf("failed to parse template for %s: %w", filename, err)
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, meta); err != nil {
		return fmt.Errorf("failed to execute template for %s: %w", filename, err)
	}

	// Run gofmt/goimports equivalent format
	formatted, err := format.Source(buf.Bytes())
	if err != nil {
		log.Printf("Warning: Failed to format source: %v. Writing raw template output.", err)
		formatted = buf.Bytes()
	}

	if err := os.WriteFile(filename, formatted, 0o644); err != nil {
		return fmt.Errorf("failed to write output file %s: %w", filename, err)
	}
	fmt.Printf("Generated %s successfully\n", filename)
	return nil
}

const loggingTemplate = `// Code generated by middlegen. DO NOT EDIT.
package {{.PackageName}}

import (
	"context"
	"log/slog"
	{{if .MiddlewareImport}}"{{.MiddlewareImport}}"{{end}}
	{{range .Imports}}{{.}}
	{{end}}
)

func {{.InterfaceNameWithoutPackage}}LoggingMiddleware() {{.MiddlewareType}}[{{.InterfaceName}}] {
	logger := slog.Default().With(slog.String("service", "{{$.ServiceName}}"))
	return func(next {{.InterfaceName}}) {{.InterfaceName}} {
		return &{{.InterfaceNameLower}}LoggingService{ {{.InterfaceNameWithoutPackage}}: next, next: next, logger: logger}
	}
}

type {{.InterfaceNameLower}}LoggingService struct {
	{{.InterfaceName}} // forwards methods of embedded interfaces undecorated
	next   {{.InterfaceName}}
	logger *slog.Logger
}

{{range .Methods}}
func (l *{{$.InterfaceNameLower}}LoggingService) {{.Name}}({{.ParamsSignature}}) {{.ResultsSignature}} {
	{{if .HasContext}}l.logger.DebugContext(ctx, "{{.Name}} started"{{if .SlogAttributes}}, {{.SlogAttributes}}{{end}}){{end}}
	{{if .Results}}{{if .HasContext}}
	{{end}}{{.ResultsVars}} := l.next.{{.Name}}({{.ParamsNames}}){{if and .HasError .HasContext}}
	if err != nil {
		l.logger.ErrorContext(ctx, "{{.Name}} failed", slog.Any("error", err))
	}{{end}}

	return {{.ResultsVars}}
	{{else}}{{if .HasContext}}
	{{end}}l.next.{{.Name}}({{.ParamsNames}})
	{{end}}
}
{{end}}
`

const tracingTemplate = `// Code generated by middlegen. DO NOT EDIT.
package {{.PackageName}}

import (
	"context"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	{{if .MiddlewareImport}}"{{.MiddlewareImport}}"{{end}}
	{{range .Imports}}{{.}}
	{{end}}
)

func {{.InterfaceNameWithoutPackage}}TracingMiddleware() {{.MiddlewareType}}[{{.InterfaceName}}] {
	tracer := otel.Tracer("{{$.ServiceName}}")
	return func(next {{.InterfaceName}}) {{.InterfaceName}} {
		return &{{.InterfaceNameLower}}TracingService{ {{.InterfaceNameWithoutPackage}}: next, next: next, tracer: tracer}
	}
}

type {{.InterfaceNameLower}}TracingService struct {
	{{.InterfaceName}} // forwards methods of embedded interfaces undecorated
	next   {{.InterfaceName}}
	tracer trace.Tracer
}

{{range .Methods}}
func (t *{{$.InterfaceNameLower}}TracingService) {{.Name}}({{.ParamsSignature}}) {{.ResultsSignature}} {
	{{if .HasContext}}ctx, span := t.tracer.Start(ctx, "{{$.ServiceName}}.{{.Name}}", trace.WithSpanKind(trace.SpanKindInternal))
	defer span.End()

	{{end}}{{if .Results}}{{if .HasContext}}
	{{end}}{{.ResultsVars}} := t.next.{{.Name}}({{.ParamsNames}}){{if and .HasError .HasContext}}
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	}{{end}}

	return {{.ResultsVars}}
	{{else}}{{if .HasContext}}
	{{end}}t.next.{{.Name}}({{.ParamsNames}})
	{{end}}
}
{{end}}
`

const metricsTemplate = `// Code generated by middlegen. DO NOT EDIT.
package {{.PackageName}}

import (
	"context"
	{{if .HasCustomAttributes}}"fmt"{{end}}
	"time"

	"go.opentelemetry.io/otel"
	{{if .CustomCounters}}"go.opentelemetry.io/otel/metric"{{end}}
	{{if .HasCustomAttributes}}"go.opentelemetry.io/otel/attribute"{{end}}
	{{if .MiddlewareImport}}"{{.MiddlewareImport}}"{{end}}
	"{{.LibraryModule}}/telemetry"
	{{range .Imports}}{{.}}
	{{end}}
)

func {{.InterfaceNameWithoutPackage}}MetricsMiddleware() {{.MiddlewareType}}[{{.InterfaceName}}] {
	meter := otel.GetMeterProvider().Meter("{{$.ServiceName}}")
	recorder := telemetry.NewMetricsRecorder(meter, "{{$.InterfaceNameWithoutPackage | toSnakeCase}}")

	{{range .CustomCounters}}
	{{.FieldName}}, err := recorder.Meter().Int64Counter("{{.MetricName}}", metric.WithDescription("Custom counter for {{.MetricName}}"))
	if err != nil {
		otel.Handle(err)
	}
	{{- end}}

	return func(next {{.InterfaceName}}) {{.InterfaceName}} {
		return &{{.InterfaceNameLower}}MetricsService{
			{{.InterfaceNameWithoutPackage}}: next,
			next:     next,
			recorder: recorder,
			{{range .CustomCounters}}
			{{.FieldName}}: {{.FieldName}},
			{{- end}}
		}
	}
}

type {{.InterfaceNameLower}}MetricsService struct {
	{{.InterfaceName}} // forwards methods of embedded interfaces undecorated
	next     {{.InterfaceName}}
	recorder *telemetry.MetricsRecorder
	{{range .CustomCounters}}
	{{.FieldName}} metric.Int64Counter
	{{- end}}
}

{{range .Methods}}
func (m *{{$.InterfaceNameLower}}MetricsService) {{.Name}}({{.ParamsSignature}}) {{.ResultsSignature}} {
	{{if .HasContext}}now := time.Now()
	{{range .CustomCounters}}m.{{.}}.Add(ctx, 1)
	{{end}}
	{{end}}
	{{if .Results}}{{if .HasContext}}
	{{end}}{{.ResultsVars}} := m.next.{{.Name}}({{.ParamsNames}}){{if .HasContext}}
	m.recorder.Observe(ctx, "{{.Name}}", now, {{if .HasError}}err{{else}}nil{{end}},
		{{range .CustomAttributes}}attribute.String("{{.Name}}", fmt.Sprintf("%v", {{.Type}})),
		{{end}}
	)
	{{end}}
	return {{.ResultsVars}}
	{{else}}{{if .HasContext}}
	{{end}}m.next.{{.Name}}({{.ParamsNames}}){{if .HasContext}}
	m.recorder.Observe(ctx, "{{.Name}}", now, nil,
		{{range .CustomAttributes}}attribute.String("{{.Name}}", fmt.Sprintf("%v", {{.Type}})),
		{{end}}
	)
	{{end}}
	{{end}}
}
{{end}}
`

// getImportName returns the qualifier an import is referenced by: its
// explicit alias, or the package name assumed from the path (last element,
// skipping a major-version suffix, a "go-" prefix, and anything after a
// character that cannot be part of an identifier, so gopkg.in/yaml.v3 is
// "yaml" and github.com/jackc/pgx/v5 is "pgx").
func getImportName(imp *ast.ImportSpec) string {
	if imp.Name != nil {
		return imp.Name.Name
	}
	return assumedImportName(strings.Trim(imp.Path.Value, `"`))
}

func assumedImportName(importPath string) string {
	base := path.Base(importPath)
	if strings.HasPrefix(base, "v") {
		if _, err := strconv.Atoi(base[1:]); err == nil {
			if dir := path.Dir(importPath); dir != "." {
				base = path.Base(dir)
			}
		}
	}
	base = strings.TrimPrefix(base, "go-")
	if i := strings.IndexFunc(base, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '_'
	}); i >= 0 {
		base = base[:i]
	}
	return base
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

func detectLibraryModule(moduleRoot string) string {
	if moduleName, err := getModuleName(moduleRoot); err == nil && moduleName == defaultLibraryModule {
		return moduleName
	}
	return defaultLibraryModule
}

func qualifyType(typeStr string, declaredTypes map[string]bool, alias string) string {
	var buf bytes.Buffer
	var currentWord bytes.Buffer

	for _, char := range typeStr {
		if (char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') || (char >= '0' && char <= '9') || char == '_' {
			currentWord.WriteRune(char)
		} else {
			word := currentWord.String()
			if declaredTypes[word] {
				buf.WriteString(alias)
				buf.WriteString(".")
				buf.WriteString(word)
			} else {
				buf.WriteString(word)
			}
			currentWord.Reset()
			buf.WriteRune(char)
		}
	}
	word := currentWord.String()
	if declaredTypes[word] {
		buf.WriteString(alias)
		buf.WriteString(".")
		buf.WriteString(word)
	} else {
		buf.WriteString(word)
	}
	return buf.String()
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

// methodDirectives holds the parsed //<prefix>:... comments of one method.
type methodDirectives struct {
	nonTransactional bool
	echo             []string
	echoNone         bool
	redact           map[string]bool
	attrs            []Field
	counters         []string
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
				parts := strings.SplitN(after, "=", 2)
				if len(parts) == 2 {
					d.attrs = append(d.attrs, Field{Name: strings.TrimSpace(parts[0]), Type: strings.TrimSpace(parts[1])})
				}
			} else if after, ok := strings.CutPrefix(arg, "counter:"); ok {
				d.counters = append(d.counters, strings.TrimSpace(after))
			}
		}
	}
	return d
}

// parseMethodComments returns the metric attributes and counters declared on
// a method. It is a thin wrapper over parseDirectives kept for callers that
// only need the metric directives.
func parseMethodComments(doc *ast.CommentGroup, line *ast.CommentGroup, prefix string) (attrs []Field, counters []string) {
	d := parseDirectives(doc, line, prefix)
	return d.attrs, d.counters
}

// splitList splits a comma- or space-separated list of names.
func splitList(s string) []string {
	var out []string
	for _, part := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || unicode.IsSpace(r) }) {
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

func toCamelCase(str string) string {
	parts := strings.Split(str, "_")
	var buf bytes.Buffer
	for i, part := range parts {
		if part == "" {
			continue
		}
		if i == 0 {
			buf.WriteString(strings.ToLower(part))
		} else {
			buf.WriteString(strings.ToUpper(part[0:1]) + part[1:])
		}
	}
	return buf.String()
}

func getZeroValue(typeStr string) string {
	if typeStr == "error" {
		return "nil"
	}
	if strings.HasPrefix(typeStr, "*") || strings.HasPrefix(typeStr, "[]") || strings.HasPrefix(typeStr, "map[") || typeStr == "any" {
		return "nil"
	}
	if typeStr == "string" {
		return `""`
	}
	if typeStr == "bool" {
		return "false"
	}
	if typeStr == "int" || typeStr == "int8" || typeStr == "int16" || typeStr == "int32" || typeStr == "int64" ||
		typeStr == "uint" || typeStr == "uint8" || typeStr == "uint16" || typeStr == "uint32" || typeStr == "uint64" ||
		typeStr == "uintptr" || typeStr == "byte" || typeStr == "rune" {
		return "0"
	}
	if typeStr == "float32" || typeStr == "float64" {
		return "0.0"
	}
	// *new(T) yields the zero value of any type, including structs,
	// interfaces, and named types, where a T(0) conversion would not compile.
	return "*new(" + typeStr + ")"
}

const uowRepoTemplate = `// Code generated by middlegen. DO NOT EDIT.
package {{.PackageName}}

import (
	"context"
	"{{.LibraryModule}}/uow"
	{{if .MiddlewareImport}}"{{.MiddlewareImport}}"{{end}}
	{{range .Imports}}{{.}}
	{{end}}
)

func {{.InterfaceNameWithoutPackage}}UoWMiddleware() {{.MiddlewareType}}[{{.InterfaceName}}] {
	return func(next {{.InterfaceName}}) {{.InterfaceName}} {
		return &{{.InterfaceNameLower}}UoWMiddleware{ {{.InterfaceNameWithoutPackage}}: next, next: next}
	}
}

type {{.InterfaceNameLower}}UoWMiddleware struct {
	{{.InterfaceName}} // forwards methods of embedded interfaces undecorated
	next {{.InterfaceName}}
}

{{range .Methods}}
func (m *{{$.InterfaceNameLower}}UoWMiddleware) {{.Name}}({{.ParamsSignature}}) {{.ResultsSignature}} {
	{{if and .HasContext (not .NonTransactional)}}if uowInstance, ok := uow.Extract(ctx); ok {
		uowInstance.Defer(func(txCtx context.Context) error {
			{{.RepoDeferStmt}}
		})
		{{.RepoEchoStmt}}
	}
	{{end}}{{if .Results}}return {{end}}m.next.{{.Name}}({{.ParamsNames}})
}
{{end}}
`

const uowServiceTemplate = `// Code generated by middlegen. DO NOT EDIT.
package {{.PackageName}}

import (
	"context"
	{{if .HasErrorlessTxMethods}}"log/slog"{{end}}
	"{{.LibraryModule}}/uow"
	{{if .MiddlewareImport}}"{{.MiddlewareImport}}"{{end}}
	{{range .Imports}}{{.}}
	{{end}}
)

func {{.InterfaceNameWithoutPackage}}UoWMiddleware(manager *uow.Manager) {{.MiddlewareType}}[{{.InterfaceName}}] {
	return func(next {{.InterfaceName}}) {{.InterfaceName}} {
		return &{{.InterfaceNameLower}}UoWMiddleware{ {{.InterfaceNameWithoutPackage}}: next, next: next, manager: manager}
	}
}

type {{.InterfaceNameLower}}UoWMiddleware struct {
	{{.InterfaceName}} // forwards methods of embedded interfaces undecorated
	next    {{.InterfaceName}}
	manager *uow.Manager
}

{{range .Methods}}
func (m *{{$.InterfaceNameLower}}UoWMiddleware) {{.Name}}({{.ParamsSignature}}) {{.ResultsSignature}} {
	{{if .HasContext}}
		{{- if and (eq (len .Results) 1) .HasError}}
		return m.manager.RunWith(ctx, func(uowCtx context.Context) error {
			return m.next.{{.Name}}({{.ParamsNamesWithUow}})
		})
		{{- else if and .Results .HasError}}
		var (
			{{range .Results}}
			{{- if ne .Type "error"}}{{.Name}} {{.Type}}
			{{end}}{{end}}err error
		)
		err = m.manager.RunWith(ctx, func(uowCtx context.Context) error {
			var innerErr error
			{{.NonErrorResultsVars}}, innerErr = m.next.{{.Name}}({{.ParamsNamesWithUow}})
			return innerErr
		})
		return {{.NonErrorResultsVars}}, err
		{{- else if .Results}}
		var (
			{{range .Results}}{{.Name}} {{.Type}}
			{{end}}
		)
		// {{.Name}} cannot return an error, so a failed Unit of Work is logged instead.
		if err := m.manager.RunWith(ctx, func(uowCtx context.Context) error {
			{{.ResultsVars}} = m.next.{{.Name}}({{.ParamsNamesWithUow}})
			return nil
		}); err != nil {
			slog.Default().ErrorContext(ctx, "{{$.InterfaceNameWithoutPackage}}.{{.Name}}: unit of work failed", slog.Any("error", err))
		}
		return {{.ResultsVars}}
		{{- else}}
		// {{.Name}} cannot return an error, so a failed Unit of Work is logged instead.
		if err := m.manager.RunWith(ctx, func(uowCtx context.Context) error {
			m.next.{{.Name}}({{.ParamsNamesWithUow}})
			return nil
		}); err != nil {
			slog.Default().ErrorContext(ctx, "{{$.InterfaceNameWithoutPackage}}.{{.Name}}: unit of work failed", slog.Any("error", err))
		}
		{{- end}}
	{{else}}
		{{if .Results}}return {{end}}m.next.{{.Name}}({{.ParamsNames}})
	{{end}}
}
{{end}}
`
