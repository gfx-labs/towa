// Package acp implements the Agent Client Protocol: JSON-RPC 2.0 over
// newline-delimited messages, with the protocol's types generated from the
// schema files in schema/.
//
// The *_gen.go files come from tools/acpgen. Regenerate them with
// "go generate ./src/acp" after changing the schema files or the generator.
package acp

//go:generate go run -C ../../tools/acpgen . -schema ../../src/acp/schema -out ../../src/acp
