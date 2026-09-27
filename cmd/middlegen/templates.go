package main

import (
	"bytes"
	"embed"
	"fmt"
	"go/format"
	"log"
	"os"
	"text/template"
)

//go:embed templates/*.go.tmpl
var templateFS embed.FS

var templates = template.Must(template.New("middlegen").Funcs(template.FuncMap{
	"toSnakeCase": toSnakeCase,
}).ParseFS(templateFS, "templates/*.go.tmpl"))

// fixedImports returns the import paths the template for kind declares
// itself, given what the interface needs. A path mapped to false is one the
// template leaves out, so it is rendered from the interface's imports when a
// signature uses it.
func fixedImports(kind string, meta *InterfaceMeta) map[string]bool {
	lib := meta.LibraryModule
	fixed := map[string]bool{meta.MiddlewareImport: true}
	switch kind {
	case "logging":
		fixed["log/slog"] = true
		fixed["context"] = meta.HasContextMethods
	case "tracing":
		fixed["go.opentelemetry.io/otel"] = true
		fixed["go.opentelemetry.io/otel/trace"] = true
		fixed["go.opentelemetry.io/otel/codes"] = meta.HasContextErrorMethods
		fixed["context"] = meta.HasContextMethods
	case "metrics":
		fixed["context"] = true
		fixed["time"] = true
		fixed["go.opentelemetry.io/otel"] = true
		fixed[lib+"/telemetry"] = true
		fixed["fmt"] = meta.HasCustomAttributes
		fixed["go.opentelemetry.io/otel/attribute"] = meta.HasCustomAttributes
		fixed["go.opentelemetry.io/otel/metric"] = len(meta.CustomCounters) > 0
	case "uow_repo":
		fixed["context"] = meta.HasContextMethods
		fixed[lib+"/uow"] = meta.HasDeferredMethods
	case "uow_service":
		fixed[lib+"/uow"] = true
		fixed["context"] = meta.HasContextMethods
		fixed["log/slog"] = meta.HasErrorlessTxMethods
	}
	return fixed
}

// generateMiddleware renders the template for kind into filename.
func generateMiddleware(g *generator, meta *InterfaceMeta, kind string, filename string) error {
	meta.Imports = g.imports.specs(fixedImports(kind, meta))

	var buf bytes.Buffer
	if err := templates.ExecuteTemplate(&buf, kind+".go.tmpl", meta); err != nil {
		return fmt.Errorf("failed to execute template for %s: %w", filename, err)
	}

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
