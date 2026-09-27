// Command middlegen type-checks a Go interface and generates decorator
// middlewares for it: logging, tracing, metrics, and Unit of Work
// boundaries for repositories and services.
//
// The full method set of the interface is decorated, including methods
// promoted from embedded interfaces declared in the same package, in other
// packages, or in the standard library. Methods that are unexported and
// declared in another package cannot be implemented from the generated
// package; they are forwarded through the embedded interface instead.
//
// Directives are comments on interface methods, in the method's doc comment
// or its trailing line comment, prefixed by -prefix (default "middlegen").
// Directives written on an embedded interface's methods apply wherever that
// interface is embedded.
//
//	//middlegen:non-transactional          run the repository method immediately, never defer it
//	//middlegen:echo <param>[, <param>...]  return these parameters, in order, as the deferred
//	                                        method's non-error results ("none" disables echoing)
//	//middlegen:redact <param>[, <param>...] log these parameters as "[REDACTED]"
//	//middlegen:metric attr:<name>=<expr>   add a metric attribute computed from an expression
//	//middlegen:metric counter:<name>       increment a custom counter on every call
//
// middlegen loads the package with go/packages, so it needs the go tool and
// a module whose dependencies are resolvable. Type errors caused by missing
// generated code are tolerated; errors inside the interface's own
// signatures are not.
package main

import (
	"flag"
	"log"
	"os"
	"strings"
)

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
