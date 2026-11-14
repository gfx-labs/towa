package core

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"os"

	acp "github.com/gfx-labs/go-acp"

	"github.com/gfx-labs/towa/src/config"
	"github.com/gfx-labs/towa/src/provider"
	"github.com/gfx-labs/towa/src/tools"
	"github.com/gfx-labs/towa/src/tools/fsops"
	"github.com/gfx-labs/towa/src/transcript"
)

type editArgs struct {
	Path  string `json:"file_path"`
	Edits []struct {
		Old string `json:"old_string"`
		New string `json:"new_string"`
		All bool   `json:"replace_all"`
	} `json:"edits"`
	// Old, New and All are one edit given without edits, as jcode's edit
	// takes it. Writing them as plain parameters spares the model writing
	// an array of objects as JSON by hand.
	Old *string `json:"old_string"`
	New *string `json:"new_string"`
	All *bool   `json:"replace_all"`
}

// operations returns the edits of a call: the edits array or the single
// edit given beside it, never both.
func (a editArgs) operations() ([]fsops.Edit, error) {
	single := a.Old != nil || a.New != nil || a.All != nil
	switch {
	case a.Edits != nil && single:
		return nil, errors.New("give either edits or old_string and new_string, not both")
	case single && (a.Old == nil || a.New == nil):
		return nil, errors.New("old_string and new_string go together: give both, or put the edit in edits")
	case single:
		return []fsops.Edit{{Old: *a.Old, New: *a.New, All: a.All != nil && *a.All}}, nil
	case a.Edits == nil:
		return nil, errors.New("give edits, or old_string and new_string for one edit")
	}
	edits := make([]fsops.Edit, len(a.Edits))
	for i, e := range a.Edits {
		edits[i] = fsops.Edit{Old: e.Old, New: e.New, All: e.All}
	}
	return edits, nil
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
				return tools.TextResult(transcript.OutcomeFailed, "Invalid write input: "+err.Error()), nil
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
			result := tools.TextResult(transcript.OutcomeCompleted, text)
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
				return tools.TextResult(transcript.OutcomeFailed, "Invalid edit input: "+err.Error()), nil
			}
			edits, err := args.operations()
			if err != nil {
				return tools.TextResult(transcript.OutcomeFailed, "Invalid edit input: "+err.Error()+"."), nil
			}
			roots := fsops.RootsOf(call.Env, cfg.Files)
			path, err := roots.Resolve(args.Path)
			if err != nil {
				return fileFailure(err), nil
			}
			if _, statErr := os.Stat(path); errors.Is(statErr, os.ErrNotExist) {
				return tools.TextResult(transcript.OutcomeFailed, "File does not exist. Use write to create a file."), nil
			}
			var change fsops.Change
			// An edit needs no read first: ApplyEdits reads the file as it is
			// now and refuses an old_string that is not in it exactly once.
			err = tracker.Write(ctx, path, func() error {
				edited, editErr := fsops.ApplyEdits(ctx, roots, cfg.Files, fsops.EditInput{Path: path, Edits: edits})
				change = edited.Change
				return editErr
			})
			if err != nil {
				return editFailure(err, edits), nil
			}
			result := tools.TextResult(transcript.OutcomeCompleted, fmt.Sprintf("The file %s has been updated successfully.", path))
			result.Content = []acp.ToolCallContent{change.Diff()}
			return result, nil
		},
	}
}

// editFailure says which edit of a call failed and why. Nothing was written:
// the edits of one call are applied together or not at all.
func editFailure(err error, edits []fsops.Edit) tools.Result {
	fileErr, ok := errors.AsType[*fsops.Error](err)
	if !ok {
		return fileFailure(err)
	}
	const unchanged = " The file was not changed."
	at := fmt.Sprintf("edits[%d]", fileErr.Edit)
	switch fileErr.Kind {
	case fsops.KindNoEdits:
		return tools.TextResult(transcript.OutcomeFailed, "edits is empty. Give at least one replacement.")
	case fsops.KindEmptyOld:
		return tools.TextResult(transcript.OutcomeFailed, at+".old_string is empty. Use write to create a file or to replace all of one."+unchanged)
	case fsops.KindSameText:
		return tools.TextResult(transcript.OutcomeFailed, at+": old_string and new_string are exactly the same."+unchanged)
	case fsops.KindNoMatch:
		text := at + ".old_string was not found in the file."
		if fileErr.Edit > 0 {
			// Each edit sees the result of the ones before it.
			text += " Edits are applied in order, so an earlier edit of this call may have changed that text."
		}
		return tools.TextResult(transcript.OutcomeFailed, text+unchanged+"\nString: "+edits[fileErr.Edit].Old)
	case fsops.KindNotUnique:
		return tools.TextResult(transcript.OutcomeFailed, fmt.Sprintf("%s.old_string occurs %d times in the file and replace_all is false. Set replace_all to replace every occurrence, or add surrounding lines so that it names one.%s", at, fileErr.Count, unchanged))
	case fsops.KindNoChange:
		return tools.TextResult(transcript.OutcomeFailed, "The edits leave the file exactly as it was.")
	default:
		return fileFailure(err)
	}
}

func fileFailure(err error) tools.Result {
	if _, unread := errors.AsType[*UnreadError](err); unread {
		return tools.TextResult(transcript.OutcomeFailed, "File has not been read yet. Read it first before writing to it.")
	}
	if _, changed := errors.AsType[*ChangedError](err); changed {
		return tools.TextResult(transcript.OutcomeFailed, "File has been modified since read, either by the user or by a linter. Read it again before attempting to write it.")
	}
	return tools.TextResult(transcript.OutcomeFailed, err.Error())
}
