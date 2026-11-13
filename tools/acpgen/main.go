// This file is modified from cmd/generate in github.com/coder/acp-go-sdk
// (commit 492745b, Apache-2.0). tools/acpgen/README.md lists the changes.

// Command acpgen writes the Go types and method tables of pkg/acp from the
// ACP schema files.
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/gfx-labs/towa/tools/acpgen/emit"
	"github.com/gfx-labs/towa/tools/acpgen/load"
)

func main() {
	var schemaDirFlag string
	var outDirFlag string
	flag.StringVar(&schemaDirFlag, "schema", "", "path to the schema directory (required)")
	flag.StringVar(&outDirFlag, "out", "", "output directory for generated go files (required)")
	flag.Parse()

	schemaDir := schemaDirFlag
	outDir := outDirFlag
	if schemaDir == "" || outDir == "" {
		fmt.Fprintln(os.Stderr, "acpgen: -schema and -out are required")
		os.Exit(2)
	}

	if err := os.MkdirAll(outDir, 0o755); err != nil {
		panic(err)
	}

	meta, err := load.ReadMeta(schemaDir)
	if err != nil {
		panic(err)
	}

	schema, err := load.ReadSchema(schemaDir)
	if err != nil {
		panic(err)
	}

	unstableMeta, unstableMetaFound, err := load.ReadMetaUnstable(schemaDir)
	if err != nil {
		panic(err)
	}
	unstableSchema, unstableSchemaFound, err := load.ReadSchemaUnstable(schemaDir)
	if err != nil {
		panic(err)
	}
	if unstableMetaFound != unstableSchemaFound {
		panic(fmt.Sprintf("unstable schema/meta mismatch: meta found=%v schema found=%v", unstableMetaFound, unstableSchemaFound))
	}
	if unstableMetaFound {
		mergedMeta, mergedSchema, err := load.MergeStableAndUnstable(meta, schema, unstableMeta, unstableSchema)
		if err != nil {
			panic(err)
		}
		meta = mergedMeta
		schema = mergedSchema
	}

	if err := emit.WriteConstantsJen(outDir, meta); err != nil {
		panic(err)
	}

	if err := emit.WriteTypesJen(outDir, schema, meta); err != nil {
		panic(err)
	}
	if err := emit.WriteDispatchJen(outDir, schema, meta); err != nil {
		panic(err)
	}

	// Emit helpers after types so they can reference generated structs.
	if err := emit.WriteHelpersJen(outDir, schema, meta); err != nil {
		panic(err)
	}
}
