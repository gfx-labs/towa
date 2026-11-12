package core

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/gfx-labs/towa/pkg/acp"
	"github.com/gfx-labs/towa/src/config"
	"github.com/gfx-labs/towa/src/provider"
	"github.com/gfx-labs/towa/src/tools"
	"github.com/gfx-labs/towa/src/tools/fsops"
)

type editArgs struct {
	Path string `json:"file_path"`
	Old  string `json:"old_string"`
	New  string `json:"new_string"`
	All  bool   `json:"replace_all"`
}

type writeArgs struct {
	Path    string `json:"file_path"`
	Content string `json:"content"`
}

func fileDescription(env tools.Env, cfg config.ToolsFiles, title string) func(jsontext.Value) (tools.Description, error) {
	return func(input jsontext.Value) (tools.Description, error) {
		desc := tools.Description{Title: title, Kind: acp.ToolKindEdit}
		var args struct {
			Path string `json:"file_path"`
		}
		if err := json.Unmarshal(input, &args); err != nil {
			return desc, nil
		}
		path, err := fsops.RootsOf(env, cfg).Resolve(args.Path)
		if err != nil {
			return desc, nil
		}
		desc.Locations = []acp.ToolCallLocation{{Path: path}}
		desc.WritePaths = []string{path}
		return desc, nil
	}
}

func writeTool(spec provider.Tool, env tools.Env, cfg config.Tools, tracker *ReadTracker) tools.Tool {
	return tools.Tool{
		Spec: spec, Effect: tools.Writes,
		Describe: fileDescription(env, cfg.Files, "Write file"),
		Run: func(ctx context.Context, call tools.Call) (tools.Result, error) {
			var args writeArgs
			if err := json.Unmarshal(call.Input, &args, json.RejectUnknownMembers(true)); err != nil {
				return textResult("Invalid write input: "+err.Error(), true), nil
			}
			roots := fsops.RootsOf(call.Env, cfg.Files)
			path, err := roots.Resolve(args.Path)
			if err != nil {
				return fileFailure(err), nil
			}
			var change fsops.Change
			write := func() error {
				var writeErr error
				change, writeErr = fsops.Write(ctx, roots, cfg.Files, fsops.WriteInput{Path: path, Content: args.Content})
				return writeErr
			}
			_, statErr := os.Stat(path)
			switch {
			case statErr == nil:
				err = tracker.Edit(ctx, path, write)
			case errors.Is(statErr, os.ErrNotExist):
				err = tracker.Write(ctx, path, write)
			default:
				err = statErr
			}
			if err != nil {
				return fileFailure(err), nil
			}
			text := fmt.Sprintf("File created successfully at: %s", path)
			if change.Existed {
				text = fmt.Sprintf("The file %s has been updated successfully.", path)
			}
			result := textResult(text, false)
			result.Content = []acp.ToolCallContent{change.Diff()}
			return result, nil
		},
	}
}

func editTool(spec provider.Tool, env tools.Env, cfg config.Tools, tracker *ReadTracker) tools.Tool {
	return tools.Tool{
		Spec: spec, Effect: tools.Writes,
		Describe: fileDescription(env, cfg.Files, "Edit file"),
		Run: func(ctx context.Context, call tools.Call) (tools.Result, error) {
			var args editArgs
			if err := json.Unmarshal(call.Input, &args, json.RejectUnknownMembers(true)); err != nil {
				return textResult("Invalid edit input: "+err.Error(), true), nil
			}
			if args.Old == args.New {
				return textResult("No changes to make: old_string and new_string are exactly the same.", true), nil
			}
			roots := fsops.RootsOf(call.Env, cfg.Files)
			path, err := roots.Resolve(args.Path)
			if err != nil {
				return fileFailure(err), nil
			}
			var change fsops.Change
			if args.Old == "" {
				// An empty old_string fills a missing or whitespace-only file.
				_, text, readErr := fsops.ReadText(ctx, roots, cfg.Files, path)
				var fileErr *fsops.Error
				switch {
				case readErr == nil && strings.TrimSpace(text) != "":
					return textResult("Cannot create new file - file already exists.", true), nil
				case readErr == nil, errors.As(readErr, &fileErr) && fileErr.Kind == fsops.KindNotFound:
					err = tracker.Write(ctx, path, func() error {
						var writeErr error
						change, writeErr = fsops.Write(ctx, roots, cfg.Files, fsops.WriteInput{Path: path, Content: args.New})
						return writeErr
					})
				default:
					err = readErr
				}
			} else {
				err = tracker.Edit(ctx, path, func() error {
					edited, editErr := fsops.ApplyEdits(ctx, roots, cfg.Files, fsops.EditInput{Path: path, Edits: []fsops.Edit{{Old: args.Old, New: args.New, All: args.All}}})
					change = edited.Change
					return editErr
				})
			}
			if err != nil {
				var fileErr *fsops.Error
				if errors.As(err, &fileErr) && fileErr.Kind == fsops.KindNoMatch {
					return textResult("String to replace not found in file.\nString: "+args.Old, true), nil
				}
				return fileFailure(err), nil
			}
			text := fmt.Sprintf("The file %s has been updated successfully.", path)
			if args.All {
				text = fmt.Sprintf("The file %s has been updated. All occurrences were successfully replaced.", path)
			}
			result := textResult(text, false)
			result.Content = []acp.ToolCallContent{change.Diff()}
			return result, nil
		},
	}
}

func fileFailure(err error) tools.Result {
	var unread *UnreadError
	var changed *ChangedError
	var fileErr *fsops.Error
	switch {
	case errors.As(err, &unread):
		return textResult("File has not been read yet. Read it first before writing to it.", true)
	case errors.As(err, &changed):
		return textResult("File has been modified since read, either by the user or by a linter. Read it again before attempting to write it.", true)
	case errors.As(err, &fileErr) && fileErr.Kind == fsops.KindNotUnique:
		return textResult(fmt.Sprintf("Found %d matches of the string to replace, but replace_all is false. To replace all occurrences, set replace_all to true. To replace only one occurrence, please provide more context to uniquely identify the instance.", fileErr.Count), true)
	default:
		return textResult(err.Error(), true)
	}
}
