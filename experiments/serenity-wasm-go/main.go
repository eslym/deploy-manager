// This is an ABI feasibility probe, not an OPAQUE server.
package main

import (
	"context"
	"fmt"
	"os"
	"sort"

	"github.com/tetratelabs/wazero"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: go run . path/to/opaque.wasm")
		os.Exit(2)
	}
	wasm, err := os.ReadFile(os.Args[1])
	if err != nil { fatal(err) }
	r := wazero.NewRuntime(context.Background())
	defer r.Close(context.Background())
	mod, err := r.CompileModule(context.Background(), wasm)
	if err != nil { fatal(err) }
	defer mod.Close(context.Background())
	imports := make([]string, 0)
	for _, f := range mod.ImportedFunctions() {
		module, name, _ := f.Import()
		imports = append(imports, module+"."+name)
	}
	sort.Strings(imports)
	exports := make([]string, 0)
	for name := range mod.ExportedFunctions() { exports = append(exports, name) }
	sort.Strings(exports)
	fmt.Printf("WASM valid: yes\nImports (%d):\n", len(imports))
	for _, name := range imports { fmt.Printf("  %s\n", name) }
	fmt.Printf("Exports (%d):\n", len(exports))
	for _, name := range exports { fmt.Printf("  %s\n", name) }
}

func fatal(err error) { fmt.Fprintln(os.Stderr, err); os.Exit(1) }
