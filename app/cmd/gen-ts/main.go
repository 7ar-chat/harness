// Command gen-ts writes the TypeScript types of the wire format (web/src/api/generated.ts) from the Go types
// the HTTP API sends: the DTOs of hosts/client, the store types they embed, the views of product (memory pages included) and the
// counters of the memory feature. The list of types is in types.go.
//
//	go run ./cmd/gen-ts            # from the repository root; `make gen`
//	go run ./cmd/gen-ts -o path    # elsewhere
//
// TestGeneratedFileIsCurrent fails when the committed file differs from what this tool writes.
package main

import (
	"flag"
	"fmt"
	"os"
)

// outFile is where the generated types live, relative to the repository root (this command runs from app/, so make gen passes -o).
const outFile = "web/src/api/generated.ts"

func render() ([]byte, error) {
	return newGenerator(enums(), extras(), overrides()).generate(roots())
}

func main() {
	out := flag.String("o", outFile, "file to write")
	flag.Parse()
	b, err := render()
	if err != nil {
		fmt.Fprintln(os.Stderr, "gen-ts:", err)
		os.Exit(1)
	}
	if err := os.WriteFile(*out, b, 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "gen-ts:", err)
		os.Exit(1)
	}
}
