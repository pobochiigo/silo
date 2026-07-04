package main

import (
	"bytes"
	"flag"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/printer"
	"go/token"
	"log"
	"os"
	"path/filepath"
	"strings"
	"text/template"
)

type Field struct {
	Name string
	Type string
	// Label preserves the original parameter name for log keys when Name
	// had to be renamed to avoid colliding with template-declared identifiers.
	Label string
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
	ZeroValues          string
	NonTransactional    bool
	RepoDeferStmt       string
}

type CounterMeta struct {
	FieldName  string
	MetricName string
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
	MiddlewareImport            string
	MiddlewareType              string
	LibraryModule               string
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

// reservedParamNames are identifiers declared by the generated middleware
// bodies (receivers and locals). Interface parameters with these names are
// renamed to avoid shadowing or redeclaration in the generated code.
var reservedParamNames = map[string]bool{
	"l": true, "t": true, "m": true,
	"err": true, "span": true, "now": true, "ctx": true,
	"logger": true, "tracer": true, "recorder": true, "next": true,
	"uowInstance": true, "txCtx": true, "uowCtx": true, "innerErr": true,
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

	if *typeFlag == "" {
		log.Fatalf("Error: -type flag is required")
	}

	kinds := strings.Split(*kindsFlag, ",")

	// Get current working directory package
	cwd, err := os.Getwd()
	if err != nil {
		log.Fatalf("Failed to get working directory: %v", err)
	}

	moduleRoot, err := findModuleRoot(cwd)
	if err != nil {
		log.Fatalf("Failed to find module root: %v", err)
	}

	targetDir := cwd
	if *dirFlag != "" {
		targetDir = filepath.Join(moduleRoot, *dirFlag)
	}

	filter := func(info os.FileInfo) bool {
		return !strings.HasSuffix(info.Name(), ".gen.go")
	}

	destPackageName := ""
	destFset := token.NewFileSet()
	//nolint:staticcheck
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
	//nolint:staticcheck
	pkgs, err := parser.ParseDir(fset, targetDir, filter, parser.ParseComments)
	if err != nil {
		log.Fatalf("Failed to parse target directory %s: %v", targetDir, err)
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
				if ts.Name.Name == *typeFlag {
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
		log.Fatalf("Interface %s not found in package %s", *typeFlag, packageName)
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

	parts := strings.Split(*typeFlag, ".")
	rawInterfaceName := parts[len(parts)-1]

	// Detect library module path
	libModule := *libraryModuleFlag
	if libModule == "" {
		libModule = detectLibraryModule(moduleRoot)
	}

	mwImport := *middlewareImportFlag
	if mwImport == "" {
		mwImport = libModule + "/middleware"
	}
	mwType := *middlewareTypeFlag

	interfaceType := targetInterface.Type.(*ast.InterfaceType)
	meta := &InterfaceMeta{
		PackageName:                 destPackageName,
		InterfaceName:               *typeFlag,
		InterfaceNameWithoutPackage: rawInterfaceName,
		InterfaceNameLower:          strings.ToLower(rawInterfaceName[0:1]) + rawInterfaceName[1:],
		ServiceName:                 *serviceFlag,
		MiddlewareImport:            mwImport,
		MiddlewareType:              mwType,
		LibraryModule:               libModule,
	}

	if isCrossPackage {
		meta.InterfaceName = alias + "." + *typeFlag
	}

	if meta.ServiceName == "" {
		meta.ServiceName = strings.ToLower(packageName)
	}

	for _, methodField := range interfaceType.Methods.List {
		if len(methodField.Names) == 0 {
			// Embedded interface (e.g. io.Closer): its methods are not
			// visible from the AST of this file, so no middleware is
			// generated for them. The generated wrappers embed the wrapped
			// implementation, so these methods are forwarded undecorated.
			log.Printf("Warning: methods of embedded interface %s in %s are forwarded without middleware; declare them explicitly to decorate them",
				getASTNodeString(fset, methodField.Type), *typeFlag)
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

		attrs, counters := parseMethodComments(methodField.Doc, methodField.Comment, *prefixFlag)
		method.CustomAttributes = attrs

		nonTransactional := false
		var comments []string
		if methodField.Doc != nil {
			for _, c := range methodField.Doc.List {
				comments = append(comments, c.Text)
			}
		}
		if methodField.Comment != nil {
			for _, c := range methodField.Comment.List {
				comments = append(comments, c.Text)
			}
		}
		targetNonTxPrefix := "//" + *prefixFlag + ":non-transactional"
		for _, comment := range comments {
			comment = strings.TrimSpace(comment)
			if strings.HasPrefix(comment, targetNonTxPrefix) {
				nonTransactional = true
				break
			}
		}
		method.NonTransactional = nonTransactional

		var counterFields []string
		for _, c := range counters {
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
					method.Params = append(method.Params, Field{Name: name, Type: typeStr, Label: label})
					paramIdx++
				}
			}
		}

		// Parse Results. Result names from the interface declaration are
		// deliberately ignored: generated bodies declare their own locals
		// (r0..rN, plus "err" for a trailing error).
		if funcType.Results != nil {
			resultIdx := 0
			for _, r := range funcType.Results.List {
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
				slogAttrs = append(slogAttrs, fmt.Sprintf("slog.Any(%q, %s)", p.Label, p.Name))
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

		var zeroValues []string
		for _, r := range method.Results {
			if r.Type == "error" {
				zeroValues = append(zeroValues, "nil")
				continue
			}

			// Try to find a matching parameter to return the exact passed instance
			matchedParam := ""
			for _, p := range method.Params {
				if p.Type == "context.Context" {
					continue
				}

				// 1. Exact match
				if p.Type == r.Type {
					matchedParam = p.Name
					break
				}
				// 2. Return type is pointer, param type is value (r.Type is "*T", p.Type is "T")
				if strings.HasPrefix(r.Type, "*") && r.Type[1:] == p.Type {
					matchedParam = "&" + p.Name
					break
				}
				// 3. Return type is value, param type is pointer (r.Type is "T", p.Type is "*T")
				if strings.HasPrefix(p.Type, "*") && p.Type[1:] == r.Type {
					matchedParam = "*" + p.Name
					break
				}
			}

			if matchedParam != "" {
				zeroValues = append(zeroValues, matchedParam)
			} else {
				zeroValues = append(zeroValues, getZeroValue(r.Type))
			}
		}
		method.ZeroValues = strings.Join(zeroValues, ", ")

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

	var imports []string
	for _, imp := range targetFile.Imports {
		impPath := imp.Path.Value
		cleanPath := strings.Trim(impPath, `"`)
		if cleanPath == "context" ||
			cleanPath == "time" ||
			cleanPath == "go.opentelemetry.io/otel/metric" ||
			cleanPath == "go.opentelemetry.io/otel/attribute" ||
			cleanPath == libModule+"/log/v2" ||
			cleanPath == libModule+"/trace" ||
			cleanPath == mwImport {
			continue
		}

		name := getImportName(imp)
		isReferenced := false
		for _, method := range meta.Methods {
			for _, p := range method.Params {
				if strings.Contains(p.Type, name+".") {
					isReferenced = true
					break
				}
			}
			if isReferenced {
				break
			}
			for _, r := range method.Results {
				if strings.Contains(r.Type, name+".") {
					isReferenced = true
					break
				}
			}
			if isReferenced {
				break
			}
		}

		if isReferenced {
			var buf bytes.Buffer
			printer.Fprint(&buf, fset, imp)
			imports = append(imports, buf.String())
		}
	}
	meta.Imports = imports

	if isCrossPackage {
		relDir, err := filepath.Rel(moduleRoot, targetDir)
		if err == nil {
			moduleName, err := getModuleName(moduleRoot)
			if err == nil {
				interfaceImport := fmt.Sprintf(`%s "%s/%s"`, alias, moduleName, filepath.ToSlash(relDir))
				meta.Imports = append(meta.Imports, interfaceImport)
			}
		}
	}

	prefix := toSnakeCase(*typeFlag)
	for _, kind := range kinds {
		kind = strings.TrimSpace(kind)
		switch kind {
		case "logging":
			generateMiddleware(meta, loggingTemplate, fmt.Sprintf("%s_logging_middleware.gen.go", prefix))
		case "tracing":
			generateMiddleware(meta, tracingTemplate, fmt.Sprintf("%s_tracing_middleware.gen.go", prefix))
		case "metrics":
			generateMiddleware(meta, metricsTemplate, fmt.Sprintf("%s_metrics_middleware.gen.go", prefix))
		case "uow_repo":
			generateMiddleware(meta, uowRepoTemplate, fmt.Sprintf("%s_uow_middleware.gen.go", prefix))
		case "uow_service":
			generateMiddleware(meta, uowServiceTemplate, fmt.Sprintf("%s_uow_middleware.gen.go", prefix))
		default:
			log.Fatalf("Unknown middleware kind: %s", kind)
		}
	}
}

func getASTNodeString(fset *token.FileSet, node ast.Node) string {
	var buf bytes.Buffer
	if err := printer.Fprint(&buf, fset, node); err != nil {
		return ""
	}
	return buf.String()
}

func generateMiddleware(meta *InterfaceMeta, tempStr string, filename string) {
	tmpl, err := template.New("middleware").Funcs(template.FuncMap{
		"toSnakeCase": toSnakeCase,
	}).Parse(tempStr)
	if err != nil {
		log.Fatalf("Failed to parse template for %s: %v", filename, err)
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, meta); err != nil {
		log.Fatalf("Failed to execute template for %s: %v", filename, err)
	}

	// Run gofmt/goimports equivalent format
	formatted, err := format.Source(buf.Bytes())
	if err != nil {
		log.Printf("Warning: Failed to format source: %v. Writing raw template output.", err)
		formatted = buf.Bytes()
	}

	if err := os.WriteFile(filename, formatted, 0600); err != nil {
		log.Fatalf("Failed to write output file %s: %v", filename, err)
	}
	fmt.Printf("Generated %s successfully\n", filename)
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
	{{if .HasContext}}l.logger.InfoContext(ctx, "{{.Name}} started"{{if .SlogAttributes}}, {{.SlogAttributes}}{{end}}){{end}}
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
	{{.FieldName}}, _ := recorder.Meter().Int64Counter("{{.MetricName}}", metric.WithDescription("Custom counter for {{.MetricName}}"))
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

func getImportName(imp *ast.ImportSpec) string {
	if imp.Name != nil {
		return imp.Name.Name
	}
	path := strings.Trim(imp.Path.Value, `"`)
	parts := strings.Split(path, "/")
	last := parts[len(parts)-1]
	if (strings.HasPrefix(last, "v") && len(last) > 1 && isDigit(last[1:])) || last == "v2" || last == "v3" {
		if len(parts) > 1 {
			last = parts[len(parts)-2]
		}
	}
	return last
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

func toSnakeCase(str string) string {
	var buf bytes.Buffer
	for i, r := range str {
		if r >= 'A' && r <= 'Z' {
			if i > 0 {
				buf.WriteByte('_')
			}
			buf.WriteRune(r + ('a' - 'A'))
		} else {
			buf.WriteRune(r)
		}
	}
	return buf.String()
}

func parseMethodComments(doc *ast.CommentGroup, line *ast.CommentGroup, prefix string) (attrs []Field, counters []string) {
	var comments []string
	if doc != nil {
		for _, c := range doc.List {
			comments = append(comments, c.Text)
		}
	}
	if line != nil {
		for _, c := range line.List {
			comments = append(comments, c.Text)
		}
	}

	targetPrefix := "//" + prefix + ":metric "
	for _, comment := range comments {
		comment = strings.TrimSpace(comment)
		if !strings.HasPrefix(comment, targetPrefix) {
			continue
		}
		expr := strings.TrimPrefix(comment, targetPrefix)
		expr = strings.TrimSpace(expr)

		if after, ok := strings.CutPrefix(expr, "attr:"); ok {
			kv := after
			parts := strings.SplitN(kv, "=", 2)
			if len(parts) == 2 {
				attrs = append(attrs, Field{Name: strings.TrimSpace(parts[0]), Type: strings.TrimSpace(parts[1])})
			}
		} else if after, ok := strings.CutPrefix(expr, "counter:"); ok {
			counterName := after
			counters = append(counters, strings.TrimSpace(counterName))
		}
	}
	return
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
		return {{.ZeroValues}}
	}
	{{end}}return m.next.{{.Name}}({{.ParamsNames}})
}
{{end}}
`

const uowServiceTemplate = `// Code generated by middlegen. DO NOT EDIT.
package {{.PackageName}}

import (
	"context"
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
		_ = m.manager.RunWith(ctx, func(uowCtx context.Context) error {
			{{.ResultsVars}} = m.next.{{.Name}}({{.ParamsNamesWithUow}})
			return nil
		})
		return {{.ResultsVars}}
		{{- else}}
		_ = m.manager.RunWith(ctx, func(uowCtx context.Context) error {
			m.next.{{.Name}}({{.ParamsNamesWithUow}})
			return nil
		})
		{{- end}}
	{{else}}
		{{if .Results}}return {{end}}m.next.{{.Name}}({{.ParamsNames}})
	{{end}}
}
{{end}}
`
