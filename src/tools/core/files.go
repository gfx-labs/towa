package core

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"os"

	"github.com/gfx-labs/towa/pkg/acp"
	"github.com/gfx-labs/towa/src/config"
	"github.com/gfx-labs/towa/src/provider"
	"github.com/gfx-labs/towa/src/tools"
	"github.com/gfx-labs/towa/src/tools/fsops"
)

type editArgs struct {
	Path  string `json:"file_path"`
	Edits []struct {
		Old string `json:"old_string"`
		New string `json:"new_string"`
		All bool   `json:"replace_all"`
	} `json:"edits"`
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
			roots := fsops.RootsOf(call.Env, cfg.Files)
			path, err := roots.Resolve(args.Path)
			if err != nil {
				return fileFailure(err), nil
			}
			if _, statErr := os.Stat(path); errors.Is(statErr, os.ErrNotExist) {
				return textResult("File does not exist. Use write to create a file.", true), nil
			}
			edits := make([]fsops.Edit, len(args.Edits))
			for i, e := range args.Edits {
				edits[i] = fsops.Edit{Old: e.Old, New: e.New, All: e.All}
			}
			var change fsops.Change
			err = tracker.Edit(ctx, path, func() error {
				edited, editErr := fsops.ApplyEdits(ctx, roots, cfg.Files, fsops.EditInput{Path: path, Edits: edits})
				change = edited.Change
				return editErr
			})
			if err != nil {
				return editFailure(err, edits), nil
			}
			result := textResult(fmt.Sprintf("The file %s has been updated successfully.", path), false)
			result.Content = []acp.ToolCallContent{change.Diff()}
			return result, nil
		},
	}
}

// editFailure says which edit of a call failed and why. Nothing was written:
// the edits of one call are applied together or not at all.
func editFailure(err error, edits []fsops.Edit) tools.Result {
	var fileErr *fsops.Error
	if !errors.As(err, &fileErr) {
		return fileFailure(err)
	}
	const unchanged = " The file was not changed."
	at := fmt.Sprintf("edits[%d]", fileErr.Edit)
	switch fileErr.Kind {
	case fsops.KindNoEdits:
		return textResult("edits is empty. Give at least one replacement.", true)
	case fsops.KindEmptyOld:
		return textResult(at+".old_string is empty. Use write to create a file or to replace all of one."+unchanged, true)
	case fsops.KindSameText:
		return textResult(at+": old_string and new_string are exactly the same."+unchanged, true)
	case fsops.KindNoMatch:
		text := at + ".old_string was not found in the file."
		if fileErr.Edit > 0 {
			// Each edit sees the result of the ones before it.
			text += " Edits are applied in order, so an earlier edit of this call may have changed that text."
		}
		return textResult(text+unchanged+"\nString: "+edits[fileErr.Edit].Old, true)
	case fsops.KindNotUnique:
		return textResult(fmt.Sprintf("%s.old_string occurs %d times in the file and replace_all is false. Set replace_all to replace every occurrence, or add surrounding lines so that it names one.%s", at, fileErr.Count, unchanged), true)
	case fsops.KindNoChange:
		return textResult("The edits leave the file exactly as it was.", true)
	default:
		return fileFailure(err)
	}
}

func fileFailure(err error) tools.Result {
	var unread *UnreadError
	var changed *ChangedError
	switch {
	case errors.As(err, &unread):
		return textResult("File has not been read yet. Read it first before writing to it.", true)
	case errors.As(err, &changed):
		return textResult("File has been modified since read, either by the user or by a linter. Read it again before attempting to write it.", true)
	default:
		return textResult(err.Error(), true)
	}
}
