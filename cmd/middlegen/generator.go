package main

import (
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"log"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"golang.org/x/tools/go/packages"
)

const loadMode = packages.NeedName | packages.NeedFiles | packages.NeedSyntax |
	packages.NeedTypes | packages.NeedTypesInfo | packages.NeedImports | packages.NeedModule

// InterfaceMeta is the data every template renders.
type InterfaceMeta struct {
	PackageName                 string
	InterfaceName               string // as referenced from the generated package (qualified when cross-package)
	InterfaceNameWithoutPackage string
	InterfaceNameLower          string
	ServiceName                 string
	Imports                     []string // computed per template by generateMiddleware
	Methods                     []Method
	CustomCounters              []CounterMeta
	MiddlewareImport            string
	MiddlewareType              string
	LibraryModule               string

	// Flags the templates use to decide which of their own imports are needed.
	HasCustomAttributes    bool // some method declares //<prefix>:metric attr
	HasContextMethods      bool // some method takes a context.Context
	HasContextErrorMethods bool // some method takes a context and returns an error
	HasDeferredMethods     bool // some method takes a context and is not non-transactional
	HasErrorlessTxMethods  bool // some method takes a context and returns no error
}

// CounterMeta describes one custom counter declared through a directive.
type CounterMeta struct {
	FieldName  string
	MetricName string
}

func (meta *InterfaceMeta) addMethod(m Method) {
	meta.Methods = append(meta.Methods, m)
	if m.HasContext {
		meta.HasContextMethods = true
		if m.HasError {
			meta.HasContextErrorMethods = true
		} else {
			meta.HasErrorlessTxMethods = true
		}
		if !m.NonTransactional {
			meta.HasDeferredMethods = true
		}
	}
	if len(m.CustomAttributes) > 0 {
		meta.HasCustomAttributes = true
	}
	for _, metric := range m.counterMetrics {
		if !slices.ContainsFunc(meta.CustomCounters, func(c CounterMeta) bool { return c.MetricName == metric }) {
			meta.CustomCounters = append(meta.CustomCounters, CounterMeta{
				FieldName:  toCamelCase(metric) + "Counter",
				MetricName: metric,
			})
		}
	}
}

// generator holds the state shared while rendering one interface.
type generator struct {
	opts     options
	pkg      *packages.Package // package declaring the interface
	fset     *token.FileSet    // file set the package was loaded with
	extFset  *token.FileSet    // file set for files of other packages parsed on demand
	astCache map[string]*ast.File
	imports  *importSet
	destPath string // import path of the package the generated code belongs to
}

// run generates the requested middlewares for the interface described by o,
// resolving paths relative to cwd.
func run(cwd string, o options) error {
	if o.TypeName == "" {
		return errors.New("-type flag is required")
	}
	if strings.Contains(o.TypeName, ".") {
		return fmt.Errorf("-type must be a bare interface name, got %q; use -dir to point at its package", o.TypeName)
	}
	if o.Prefix == "" {
		o.Prefix = "middlegen"
	}
	if o.MiddlewareType == "" {
		o.MiddlewareType = "middleware.Middleware"
	}
	mwQualifier, _, ok := strings.Cut(o.MiddlewareType, ".")
	if !ok || mwQualifier == "" {
		return fmt.Errorf("-middleware-type must be written as <package>.<Type>, got %q", o.MiddlewareType)
	}
	kinds := make([]string, 0, len(o.Kinds))
	for _, kind := range o.Kinds {
		if kind = strings.TrimSpace(kind); kind != "" {
			kinds = append(kinds, kind)
		}
	}
	if len(kinds) == 0 {
		return errors.New("-kinds must name at least one middleware kind")
	}
	for _, kind := range kinds {
		if _, ok := outputFileSuffix[kind]; !ok {
			return fmt.Errorf("unknown middleware kind: %s", kind)
		}
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

	libModule := o.LibraryModule
	if libModule == "" {
		libModule = detectLibraryModule(moduleRoot)
	}
	mwImport := o.MiddlewareImport
	if mwImport == "" {
		mwImport = libModule + "/middleware"
	}

	fset := token.NewFileSet()
	pkg, err := loadPackage(targetDir, fset)
	if err != nil {
		return err
	}

	obj := pkg.Types.Scope().Lookup(o.TypeName)
	if obj == nil {
		return fmt.Errorf("interface %s not found in package %s (%s)", o.TypeName, pkg.Name, targetDir)
	}
	tn, ok := obj.(*types.TypeName)
	if !ok {
		return fmt.Errorf("%s is not a type", o.TypeName)
	}
	if _, ok := tn.Type().Underlying().(*types.Interface); !ok {
		return fmt.Errorf("%s is not an interface", o.TypeName)
	}
	if named, ok := tn.Type().(*types.Named); ok && named.TypeParams().Len() > 0 {
		return fmt.Errorf("interface %s has type parameters; middlegen does not support generic interfaces", o.TypeName)
	}

	destPath, destName := destinationPackage(cwd, targetDir, pkg, moduleRoot)

	g := &generator{
		opts:     o,
		pkg:      pkg,
		fset:     fset,
		extFset:  token.NewFileSet(),
		astCache: map[string]*ast.File{},
		imports:  newImportSet(destPath, fixedImportNames(libModule, mwImport, mwQualifier)),
		destPath: destPath,
	}

	name := tn.Name()
	meta := &InterfaceMeta{
		PackageName:                 destName,
		InterfaceName:               g.imports.qualifiedName(pkg.Types, name),
		InterfaceNameWithoutPackage: name,
		InterfaceNameLower:          strings.ToLower(name[:1]) + name[1:],
		ServiceName:                 o.Service,
		MiddlewareImport:            mwImport,
		MiddlewareType:              o.MiddlewareType,
		LibraryModule:               libModule,
	}
	if meta.ServiceName == "" {
		meta.ServiceName = strings.ToLower(pkg.Name)
	}

	funcs, err := g.collectMethodsOf(tn.Type(), map[string]bool{})
	if err != nil {
		return fmt.Errorf("interface %s: %w", name, err)
	}
	if len(funcs) == 0 {
		log.Printf("Warning: interface %s has no methods to decorate", name)
	}

	wantsUoWService := slices.Contains(kinds, "uow_service")
	for _, fn := range funcs {
		if !fn.Exported() && fn.Pkg() != nil && fn.Pkg().Path() != destPath {
			log.Printf("Warning: %s.%s is unexported in package %s and cannot be decorated from package %s; it is forwarded undecorated",
				name, fn.Name(), fn.Pkg().Path(), destPath)
			continue
		}
		method, err := g.buildMethod(fn, name)
		if err != nil {
			return err
		}
		if wantsUoWService && method.HasContext && !method.HasError {
			log.Printf("Warning: %s.%s returns no error; Unit of Work failures in the generated uow_service middleware are logged via slog, not returned",
				name, method.Name)
		}
		meta.addMethod(method)
	}

	prefix := toSnakeCase(o.TypeName)
	for _, kind := range kinds {
		filename := filepath.Join(outDir, prefix+outputFileSuffix[kind])
		if err := generateMiddleware(g, meta, kind, filename); err != nil {
			return err
		}
	}
	return nil
}

// outputFileSuffix maps a middleware kind to the suffix of its output file.
var outputFileSuffix = map[string]string{
	"logging":     "_logging_middleware.gen.go",
	"tracing":     "_tracing_middleware.gen.go",
	"metrics":     "_metrics_middleware.gen.go",
	"uow_repo":    "_uow_middleware.gen.go",
	"uow_service": "_uow_middleware.gen.go",
}

// loadPackage type-checks the package in dir. Previously generated files are
// parsed as empty so stale output never breaks regeneration; the resulting
// type errors are reported as warnings and tolerated.
func loadPackage(dir string, fset *token.FileSet) (*packages.Package, error) {
	cfg := &packages.Config{
		Mode:      loadMode,
		Dir:       dir,
		Fset:      fset,
		ParseFile: parseSkippingGenerated,
	}
	pkgs, err := packages.Load(cfg, ".")
	if err != nil {
		return nil, fmt.Errorf("failed to load package in %s: %w", dir, err)
	}
	if len(pkgs) != 1 {
		return nil, fmt.Errorf("expected one package in %s, found %d", dir, len(pkgs))
	}
	pkg := pkgs[0]
	for _, e := range pkg.Errors {
		log.Printf("Warning: %v", e)
	}
	if pkg.Types == nil || pkg.TypesInfo == nil {
		return nil, fmt.Errorf("could not type-check the package in %s; fix the errors above", dir)
	}
	return pkg, nil
}

func parseSkippingGenerated(fset *token.FileSet, filename string, src []byte) (*ast.File, error) {
	mode := parser.AllErrors | parser.ParseComments
	if strings.HasSuffix(filename, ".gen.go") {
		mode = parser.PackageClauseOnly
	}
	return parser.ParseFile(fset, filename, src, mode)
}

// destinationPackage returns the import path and package clause of the
// package the generated files belong to: the interface's own package, or,
// with -dir, the package in the working directory.
func destinationPackage(cwd, targetDir string, pkg *packages.Package, moduleRoot string) (importPath, name string) {
	if sameDir(cwd, targetDir) {
		return pkg.PkgPath, pkg.Name
	}
	dest, err := packages.Load(&packages.Config{Mode: packages.NeedName, Dir: cwd}, ".")
	if err == nil && len(dest) == 1 && dest[0].Name != "" && dest[0].PkgPath != "" {
		return dest[0].PkgPath, dest[0].Name
	}
	// No Go files yet: derive the import path from the module.
	name = filepath.Base(cwd)
	importPath = name
	if module, err := getModuleName(moduleRoot); err == nil {
		if rel, err := filepath.Rel(moduleRoot, cwd); err == nil {
			if rel == "." {
				importPath = module
			} else {
				importPath = module + "/" + filepath.ToSlash(rel)
			}
		}
	}
	return importPath, name
}

func sameDir(a, b string) bool {
	if filepath.Clean(a) == filepath.Clean(b) {
		return true
	}
	fa, errA := os.Stat(a)
	fb, errB := os.Stat(b)
	return errA == nil && errB == nil && os.SameFile(fa, fb)
}

// collectMethodsOf returns the full method set of interface type t in a
// stable order: for interfaces declared in the loaded package, source order
// with embedded interfaces expanded in place; otherwise explicit methods by
// position followed by embedded interfaces in declaration order. seen
// de-duplicates methods promoted through several embeddings.
func (g *generator) collectMethodsOf(t types.Type, seen map[string]bool) ([]*types.Func, error) {
	if named, ok := types.Unalias(t).(*types.Named); ok {
		if obj := named.Obj(); obj.Pkg() != nil && obj.Pkg().Path() == g.pkg.PkgPath {
			if it := g.interfaceASTOf(obj); it != nil {
				return g.collectFromAST(it, seen)
			}
		}
	}
	iface, ok := types.Unalias(t).Underlying().(*types.Interface)
	if !ok {
		return nil, fmt.Errorf("embedded element %s is not an interface", types.TypeString(t, nil))
	}
	return g.collectFromTypes(iface, seen)
}

func (g *generator) collectFromAST(it *ast.InterfaceType, seen map[string]bool) ([]*types.Func, error) {
	var out []*types.Func
	for _, field := range it.Methods.List {
		if len(field.Names) > 0 {
			for _, ident := range field.Names {
				fn, ok := g.pkg.TypesInfo.Defs[ident].(*types.Func)
				if !ok {
					return nil, fmt.Errorf("method %s could not be type-checked", ident.Name)
				}
				if !seen[fn.Name()] {
					seen[fn.Name()] = true
					out = append(out, fn)
				}
			}
			continue
		}
		embedded := g.pkg.TypesInfo.TypeOf(field.Type)
		if embedded == nil {
			return nil, fmt.Errorf("embedded interface %s could not be resolved", getASTNodeString(g.fset, field.Type))
		}
		nested, err := g.collectMethodsOf(embedded, seen)
		if err != nil {
			return nil, err
		}
		out = append(out, nested...)
	}
	return out, nil
}

func (g *generator) collectFromTypes(iface *types.Interface, seen map[string]bool) ([]*types.Func, error) {
	var out []*types.Func
	explicit := make([]*types.Func, 0, iface.NumExplicitMethods())
	for i := 0; i < iface.NumExplicitMethods(); i++ {
		explicit = append(explicit, iface.ExplicitMethod(i))
	}
	sort.SliceStable(explicit, func(i, j int) bool { return explicit[i].Pos() < explicit[j].Pos() })
	for _, fn := range explicit {
		if !seen[fn.Name()] {
			seen[fn.Name()] = true
			out = append(out, fn)
		}
	}
	for i := 0; i < iface.NumEmbeddeds(); i++ {
		nested, err := g.collectMethodsOf(iface.EmbeddedType(i), seen)
		if err != nil {
			return nil, err
		}
		out = append(out, nested...)
	}
	return out, nil
}

// interfaceASTOf finds the declaration of tn in the loaded package's syntax.
func (g *generator) interfaceASTOf(tn *types.TypeName) *ast.InterfaceType {
	for _, file := range g.pkg.Syntax {
		for _, decl := range file.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok || gd.Tok != token.TYPE {
				continue
			}
			for _, spec := range gd.Specs {
				ts, ok := spec.(*ast.TypeSpec)
				if !ok || g.pkg.TypesInfo.Defs[ts.Name] != tn {
					continue
				}
				if it, ok := ts.Type.(*ast.InterfaceType); ok {
					return it
				}
				return nil
			}
		}
	}
	return nil
}

// typeString renders t as it must appear in the generated package,
// registering the imports it needs.
func (g *generator) typeString(t types.Type) string {
	return types.TypeString(t, g.imports.qualifier)
}

// paramName renames parameters that would collide with identifiers declared
// by the templates or with the package qualifiers used in the same method.
func (g *generator) paramName(name string) string {
	if g.imports.isName(name) {
		return name + "Arg"
	}
	return sanitizeParamName(name)
}

func isContextType(t types.Type) bool {
	named, ok := types.Unalias(t).(*types.Named)
	if !ok {
		return false
	}
	obj := named.Obj()
	return obj.Pkg() != nil && obj.Pkg().Path() == "context" && obj.Name() == "Context"
}

var universeError = types.Universe.Lookup("error").Type()

func isErrorType(t types.Type) bool {
	return types.Identical(t, universeError)
}

func hasInvalidType(t types.Type) bool {
	return strings.Contains(types.TypeString(t, nil), "invalid type")
}

func getASTNodeString(fset *token.FileSet, node ast.Node) string {
	var buf strings.Builder
	if err := printNode(&buf, fset, node); err != nil {
		return ""
	}
	return buf.String()
}
