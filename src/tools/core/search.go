package core

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"path/filepath"
	"slices"
	"strings"

	"github.com/gfx-labs/towa/pkg/acp"
	"github.com/gfx-labs/towa/src/config"
	"github.com/gfx-labs/towa/src/provider"
	"github.com/gfx-labs/towa/src/tools"
	"github.com/gfx-labs/towa/src/tools/fsops"
	"github.com/gfx-labs/towa/src/tools/search"
)

func searchDescription(env tools.Env, cfg config.ToolsFiles, name string) func(jsontext.Value) (tools.Description, error) {
	return func(input jsontext.Value) (tools.Description, error) {
		desc := tools.Description{Title: name, Kind: acp.ToolKindSearch}
		var args struct {
			Path string `json:"path"`
		}
		if err := json.Unmarshal(input, &args); err != nil {
			return desc, nil
		}
		if args.Path == "" {
			args.Path = env.Cwd
		}
		path, err := fsops.RootsOf(env, cfg).Resolve(args.Path)
		if err == nil {
			desc.Locations = []acp.ToolCallLocation{{Path: path}}
		}
		return desc, nil
	}
}

// shownPath returns how a found path is written for the model. A file under
// the working directory is written relative to it: the directory is the same
// on every line of a result, and the file tools take a relative path from
// it. Any other file keeps its absolute path.
func shownPath(env tools.Env, cfg config.ToolsFiles) (func(string) string, error) {
	// A search prints paths with links followed, so the directory is too.
	cwd, err := fsops.RootsOf(env, cfg).Resolve(env.Cwd)
	if err != nil {
		return nil, err
	}
	return func(path string) string {
		if rel, err := filepath.Rel(cwd, path); err == nil && filepath.IsLocal(rel) {
			return rel
		}
		return path
	}, nil
}

func globTool(spec provider.Tool, env tools.Env, cfg config.Tools) tools.Tool {
	return tools.Tool{
		Spec: spec, Effect: tools.ReadOnly,
		Describe: searchDescription(env, cfg.Files, "Find files"),
		Run: func(ctx context.Context, call tools.Call) (tools.Result, error) {
			var args struct {
				Pattern string `json:"pattern"`
				Path    string `json:"path"`
			}
			if err := json.Unmarshal(call.Input, &args, json.RejectUnknownMembers(true)); err != nil {
				return textResult("Invalid glob input: "+err.Error(), true), nil
			}
			found, err := search.Find(ctx, call.Env, cfg, search.Filter{Path: args.Path, Globs: []string{args.Pattern}})
			if err != nil {
				return textResult(err.Error(), true), nil
			}
			shown, err := shownPath(call.Env, cfg.Files)
			if err != nil {
				return textResult(err.Error(), true), nil
			}
			slices.SortStableFunc(found.Files, func(a, b search.File) int { return b.ModTime.Compare(a.ModTime) })
			var lines []string
			for _, file := range found.Files {
				lines = append(lines, shown(file.Path))
			}
			if len(lines) == 0 {
				lines = append(lines, "No files found")
			}
			if found.Cut {
				lines = append(lines, "[Results stopped at the configured output byte limit. Narrow the search.]")
			}
			if found.Unreadable {
				lines = append(lines, "[Some files could not be read.]")
			}
			return textResult(strings.Join(lines, "\n"), false), nil
		},
	}
}

type grepArgs struct {
	Pattern    string `json:"pattern"`
	Path       string `json:"path"`
	Glob       string `json:"glob"`
	Mode       string `json:"output_mode"`
	Before     int    `json:"-B"`
	After      int    `json:"-A"`
	Context    *int   `json:"context"`
	Numbers    *bool  `json:"-n"`
	IgnoreCase bool   `json:"-i"`
	Type       string `json:"type"`
	Limit      *int   `json:"head_limit"`
	Offset     int    `json:"offset"`
	Multiline  bool   `json:"multiline"`
}

// grepDefaultLimit is the head_limit of a call that names none, as the
// tool's schema says.
const grepDefaultLimit = 250

func grepTool(spec provider.Tool, env tools.Env, cfg config.Tools) tools.Tool {
	return tools.Tool{
		Spec: spec, Effect: tools.ReadOnly,
		Describe: searchDescription(env, cfg.Files, "Search content"),
		Run: func(ctx context.Context, call tools.Call) (tools.Result, error) {
			var args grepArgs
			if err := json.Unmarshal(call.Input, &args, json.RejectUnknownMembers(true)); err != nil {
				return textResult("Invalid grep input: "+err.Error(), true), nil
			}
			limit := grepDefaultLimit
			if args.Limit != nil {
				limit = *args.Limit
			}
			in := search.GrepInput{Filter: search.Filter{Path: args.Path, Limit: limit, Offset: args.Offset}, Pattern: args.Pattern, IgnoreCase: args.IgnoreCase, Multiline: args.Multiline}
			if args.Glob != "" {
				in.Globs = []string{args.Glob}
			}
			if args.Type != "" {
				in.Types = []string{args.Type}
			}
			switch args.Mode {
			case "", "files_with_matches":
				in.Output = search.Files
			case "count":
				in.Output = search.Counts
			case "content":
				in.Output, in.Before, in.After = search.Lines, args.Before, args.After
				if args.Context != nil {
					in.Before, in.After = *args.Context, *args.Context
				}
			default:
				return textResult("Unknown output_mode: "+args.Mode, true), nil
			}
			found, err := search.Grep(ctx, call.Env, cfg, in)
			if err != nil {
				return textResult(err.Error(), true), nil
			}
			shown, err := shownPath(call.Env, cfg.Files)
			if err != nil {
				return textResult(err.Error(), true), nil
			}
			var lines []string
			switch in.Output {
			case search.Lines:
				for _, line := range found.Lines {
					separator := ":"
					if line.Context {
						separator = "-"
					}
					if args.Numbers == nil || *args.Numbers {
						lines = append(lines, fmt.Sprintf("%s%s%d%s%s", shown(line.Path), separator, line.Number, separator, line.Text))
					} else {
						lines = append(lines, shown(line.Path)+separator+line.Text)
					}
				}
			case search.Files:
				for _, file := range found.Files {
					lines = append(lines, shown(file.Path))
				}
				if len(lines) > 0 {
					lines = append([]string{fmt.Sprintf("Found %d files", len(found.Files))}, lines...)
				}
			case search.Counts:
				for _, file := range found.Files {
					lines = append(lines, fmt.Sprintf("%s:%d", shown(file.Path), file.Count))
				}
			}
			if len(lines) == 0 {
				text := "No matches found"
				if in.Output == search.Files {
					text = "No files found"
				}
				lines = append(lines, text)
			}
			if found.More {
				lines = append(lines, "[More results exist. Increase head_limit or use offset.]")
			}
			if found.Cut {
				lines = append(lines, "[Results stopped at the configured output byte limit. Narrow the search.]")
			}
			if found.Unreadable {
				lines = append(lines, "[Some files could not be read.]")
			}
			return textResult(strings.Join(lines, "\n"), false), nil
		},
	}
}
