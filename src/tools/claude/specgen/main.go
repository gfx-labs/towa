// Command specgen deploys the selected pinned Claude tool captures unchanged.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"fmt"
	"os"
	"path/filepath"
)

func main() {
	if err := generate(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func generate() error {
	root := filepath.Join("..", "..", "..", "prompts", "claude-code", "2.1.280")
	manifest, err := os.ReadFile(filepath.Join(root, "manifest.json"))
	if err != nil {
		return err
	}
	var index struct {
		Files []struct {
			Path   string `json:"path"`
			SHA256 string `json:"sha256"`
		} `json:"files"`
	}
	if err := json.Unmarshal(manifest, &index); err != nil {
		return err
	}
	hashes := make(map[string]string)
	for _, entry := range index.Files {
		hashes[entry.Path] = entry.SHA256
	}
	if err := os.MkdirAll("specs", 0o755); err != nil {
		return err
	}
	// v1 selects captured lean descriptions with read-before-edit retained.
	// This is an explicit deployment choice, not a measured upstream default.
	for _, name := range []string{"Edit", "Write", "Glob", "Grep", "TodoWrite"} {
		path := "tools/" + name + ".v1.json"
		data, err := os.ReadFile(filepath.Join(root, path))
		if err != nil {
			return err
		}
		digest := sha256.Sum256(data)
		if hashes[path] != hex.EncodeToString(digest[:]) {
			return fmt.Errorf("capture checksum mismatch: %s", path)
		}
		if err := os.WriteFile(filepath.Join("specs", name+".json"), data, 0o644); err != nil {
			return err
		}
	}
	profile := filepath.Join(root, "profiles", "print-sdk.default.claude-opus-5-5")
	manifest, err = os.ReadFile(filepath.Join(profile, "manifest.json"))
	if err != nil {
		return err
	}
	if err := json.Unmarshal(manifest, &index); err != nil {
		return err
	}
	hashes = make(map[string]string)
	for _, entry := range index.Files {
		hashes[entry.Path] = entry.SHA256
	}
	for _, name := range []string{"Read", "Bash"} {
		path := "tools/" + name + ".json"
		data, err := os.ReadFile(filepath.Join(profile, path))
		if err != nil {
			return err
		}
		digest := sha256.Sum256(data)
		if hashes[path] != hex.EncodeToString(digest[:]) {
			return fmt.Errorf("profile capture checksum mismatch: %s", path)
		}
		if err := os.WriteFile(filepath.Join("specs", name+".json"), data, 0o644); err != nil {
			return err
		}
	}
	return nil
}
