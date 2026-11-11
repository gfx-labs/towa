//go:build ignore

// Generates deployment strings from the canonical, hash-verified Codex capture.
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/json/v2"
	"flag"
	"fmt"
	"go/format"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

const (
	capturePath = "prompts/codex/ce0323f2"
	commit      = "ce0323f2f2d3b25e82806cd555237da3db4e1897"
	catalogPath = "codex-rs/models-manager/models.json"
)

type capturedFile struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Bytes  int    `json:"bytes"`
	Source struct {
		Kind    string `json:"kind"`
		Path    string `json:"path"`
		Pointer string `json:"json_pointer"`
	} `json:"source"`
}

type capture struct {
	Commit       string         `json:"commit"`
	Files        []capturedFile `json:"files"`
	Interruption map[string]struct {
		File string `json:"file"`
		Role string `json:"role"`
	} `json:"interruption"`
}

type model struct {
	Slug              string  `json:"slug"`
	MultiAgentVersion *string `json:"multi_agent_version"`
	Messages          struct {
		Template *string `json:"instructions_template"`
		Tools    any     `json:"tools"`
	} `json:"model_messages"`
}

type deployment struct {
	Model            string
	TemplatePath     string
	Pointer          string
	System           string
	InterruptionPath string
	Interruption     string
	InterruptionRole string
}

func main() {
	root := flag.String("root", ".", "repository root")
	check := flag.Bool("check", false, "fail if deployment differs, without writing")
	flag.Parse()
	if err := run(*root, *check); err != nil {
		fmt.Fprintln(os.Stderr, "capture/prompts:", err)
		os.Exit(1)
	}
}

func run(root string, check bool) error {
	base := filepath.Join(root, capturePath)
	manifestBytes, err := os.ReadFile(filepath.Join(base, "manifest.json"))
	if err != nil {
		return err
	}
	var manifest capture
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		return err
	}
	if manifest.Commit != commit {
		return fmt.Errorf("capture commit %q, expected %q", manifest.Commit, commit)
	}
	files := make(map[string]capturedFile, len(manifest.Files))
	for _, file := range manifest.Files {
		if !fs.ValidPath(file.Path) {
			return fmt.Errorf("invalid capture path %q", file.Path)
		}
		if _, exists := files[file.Path]; exists {
			return fmt.Errorf("duplicate capture path %q", file.Path)
		}
		files[file.Path] = file
	}
	read := func(path string) (string, error) {
		file, ok := files[path]
		if !ok {
			return "", fmt.Errorf("capture manifest has no %q", path)
		}
		data, err := os.ReadFile(filepath.Join(base, filepath.FromSlash(path)))
		if err != nil {
			return "", err
		}
		if len(data) != file.Bytes || fmt.Sprintf("%x", sha256.Sum256(data)) != file.SHA256 {
			return "", fmt.Errorf("capture bytes or SHA256 differ for %q", path)
		}
		return string(data), nil
	}
	catalog, err := read("source/" + catalogPath)
	if err != nil {
		return err
	}
	var metadata struct {
		Models []model `json:"models"`
	}
	if err := json.Unmarshal([]byte(catalog), &metadata); err != nil {
		return err
	}
	if len(metadata.Models) == 0 {
		return fmt.Errorf("captured catalog has no models")
	}
	// The selector is captured at core/src/tasks/mod.rs:85-98: v2 uses the
	// developer marker, other effective versions use the contextual user one.
	if _, err := read("source/codex-rs/core/src/tasks/mod.rs"); err != nil {
		return err
	}
	seen := make(map[string]bool, len(metadata.Models))
	deployed := make([]deployment, 0, len(metadata.Models))
	for index, item := range metadata.Models {
		if item.Slug == "" || strings.ContainsAny(item.Slug, `/\\`) || item.Slug == "." || item.Slug == ".." || seen[item.Slug] {
			return fmt.Errorf("invalid or duplicate catalog model %q", item.Slug)
		}
		seen[item.Slug] = true
		if item.Messages.Template == nil || *item.Messages.Template == "" {
			return fmt.Errorf("model %q has no captured instructions_template", item.Slug)
		}
		if item.Messages.Tools != nil {
			return fmt.Errorf("model %q has tool messages without a deployed text mapping", item.Slug)
		}
		path := "instructions/" + item.Slug + ".instructions_template.md"
		system, err := read(path)
		if err != nil {
			return err
		}
		pointer := "/models/" + strconv.Itoa(index) + "/model_messages/instructions_template"
		source := files[path].Source
		if source.Kind != "json_string" || source.Path != catalogPath || source.Pointer != pointer || system != *item.Messages.Template {
			return fmt.Errorf("model %q instructions do not match captured catalog provenance", item.Slug)
		}
		variant := "contextual_user"
		if item.MultiAgentVersion != nil {
			switch *item.MultiAgentVersion {
			case "v2":
				variant = "developer"
			case "v1", "disabled":
			default:
				return fmt.Errorf("model %q has unsupported multi_agent_version %q", item.Slug, *item.MultiAgentVersion)
			}
		}
		marker, ok := manifest.Interruption[variant]
		if !ok {
			return fmt.Errorf("model %q lacks interruption variant %q", item.Slug, variant)
		}
		wantRole := "user"
		if variant == "developer" {
			wantRole = "developer"
		}
		if marker.Role != wantRole {
			return fmt.Errorf("interruption variant %q has role %q, expected %q", variant, marker.Role, wantRole)
		}
		interruption, err := read(marker.File)
		if err != nil {
			return err
		}
		deployed = append(deployed, deployment{
			Model: item.Slug, TemplatePath: path, Pointer: pointer, System: system,
			InterruptionPath: marker.File, Interruption: interruption, InterruptionRole: marker.Role,
		})
	}
	sort.Slice(deployed, func(i, j int) bool { return deployed[i].Model < deployed[j].Model })
	var output bytes.Buffer
	fmt.Fprintln(&output, "// Code generated by tools/capture/prompts.go. DO NOT EDIT.")
	fmt.Fprintln(&output, "// Canonical capture: "+capturePath+"/manifest.json")
	fmt.Fprintln(&output, "package prompts")
	fmt.Fprintf(&output, "\nconst codexCommit = %q\n", manifest.Commit)
	fmt.Fprintln(&output, "\nvar codexPrompts = map[string]Prompt{")
	for _, item := range deployed {
		fmt.Fprintf(&output, "// %s#%s\n", catalogPath, item.Pointer)
		fmt.Fprintf(&output, "// %s (sha256 %s)\n", item.TemplatePath, files[item.TemplatePath].SHA256)
		fmt.Fprintf(&output, "// %s (sha256 %s)\n", item.InterruptionPath, files[item.InterruptionPath].SHA256)
		fmt.Fprintf(&output, "%q: {System: %q, Interruption: %q, InterruptionRole: %q},\n", item.Model, item.System, item.Interruption, item.InterruptionRole)
	}
	fmt.Fprintln(&output, "}")
	formatted, err := format.Source(output.Bytes())
	if err != nil {
		return err
	}
	out := filepath.Join(root, "src/prompts/codex_gen.go")
	if check {
		existing, err := os.ReadFile(out)
		if err != nil {
			return err
		}
		if !bytes.Equal(existing, formatted) {
			return fmt.Errorf("%s differs: run go generate ./src/prompts", out)
		}
		return nil
	}
	return os.WriteFile(out, formatted, 0o644)
}
