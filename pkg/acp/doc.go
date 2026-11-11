// Package acp implements the Agent Client Protocol: JSON-RPC 2.0 over
// newline-delimited messages, with the protocol's types generated from the
// schema files in schema/.
//
// The *_gen.go files come from tools/acpgen. Regenerate them with
// "go generate ./pkg/acp" after changing the schema files or the generator.
package acp

//go:generate go run -C ../../tools/acpgen . -schema ../../pkg/acp/schema -out ../../pkg/acp
