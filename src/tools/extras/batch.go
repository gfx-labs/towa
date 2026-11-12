package extras

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"

	"github.com/gfx-labs/acpharness/pkg/acp"
	"github.com/gfx-labs/acpharness/src/config"
	"github.com/gfx-labs/acpharness/src/loop"
	"github.com/gfx-labs/acpharness/src/tools"
	"github.com/gfx-labs/acpharness/src/tools/batch"
	"github.com/gfx-labs/acpharness/src/transcript"
)

// Batch takes the loop's dispatcher lookup, normally loop.BatchDispatcher.
// It never invokes a child tool directly, bypassing permission or gating.
func Batch(cfg config.ToolsBatch, dispatcher func(context.Context) (batch.Dispatcher, bool)) (tools.Tool, error) {
	if !cfg.Enabled {
		return tools.Tool{}, fmt.Errorf("batch: %w", ErrDisabled)
	}
	if dispatcher == nil {
		return tools.Tool{}, errors.New("batch: no dispatcher lookup")
	}
	if cfg.MaxCalls <= 0 {
		return tools.Tool{}, errors.New("batch: max calls must be positive")
	}
	schema, err := json.Marshal(map[string]any{"type": "object", "required": []string{"tool_calls"}, "properties": map[string]any{
		"intent":     map[string]any{"type": "string"},
		"tool_calls": map[string]any{"type": "array", "minItems": 1, "maxItems": cfg.MaxCalls, "items": map[string]any{"type": "object", "required": []string{"tool", "intent"}, "properties": map[string]any{"tool": map[string]any{"type": "string"}, "intent": map[string]any{"type": "string"}}, "additionalProperties": true}},
	}})
	if err != nil {
		return tools.Tool{}, err
	}
	t := newTool(NameBatch, "Run several independent tool calls in one call and return every result. Each entry contains tool, intent and the tool's normal arguments as sibling fields. Calls that only read run at the same time, and a call that writes or executes runs in the order given. Do not batch calls that depend on each other's results, and do not nest batch.", acp.ToolKindExecute, schema)
	// Children can execute. Serialize the parent against outer calls as well.
	t.Effect = tools.Executes
	t.Run = func(ctx context.Context, call tools.Call) (tools.Result, error) {
		var in struct {
			Calls []map[string]jsontext.Value `json:"tool_calls"`
		}
		if err := json.Unmarshal(call.Input, &in); err != nil {
			return tools.Result{}, err
		}
		calls := make([]batch.Call, 0, len(in.Calls))
		for _, raw := range in.Calls {
			var name string
			if err := json.Unmarshal(raw["tool"], &name); err != nil {
				return tools.Result{}, err
			}
			if name == NameBatch {
				return textResult(transcript.OutcomeFailed, "Cannot batch the 'batch' tool"), nil
			}
			delete(raw, "tool")
			input, err := json.Marshal(raw)
			if err != nil {
				return tools.Result{}, err
			}
			calls = append(calls, batch.Call{Name: name, Input: input})
		}
		d, ok := dispatcher(ctx)
		if !ok || d.Plan == nil || d.Run == nil {
			return tools.Result{}, errors.New("batch: no dispatcher for this call")
		}
		items, err := batch.Run(ctx, cfg, d, calls)
		if err != nil {
			var e *batch.Error
			if !errors.As(err, &e) {
				return tools.Result{}, err
			}
			text := "No tool calls provided"
			if e.Kind == batch.KindTooMany {
				text = fmt.Sprintf("Maximum %d tool calls allowed", e.Limit)
			}
			return textResult(transcript.OutcomeFailed, text), nil
		}
		result := tools.Result{Outcome: batch.Outcome(items)}
		succeeded, failed := 0, 0
		for i, item := range items {
			heading := fmt.Sprintf("--- [%d] %s ---\n", i+1, calls[i].Name)
			result.Parts = append(result.Parts, transcript.Part{Text: heading})
			result.Content = append(result.Content, tools.TextContent(heading))
			if item.Err != nil {
				failed++
				text := fmt.Sprintf("Error: %v\n\n", item.Err)
				result.Parts = append(result.Parts, transcript.Part{Text: text})
				result.Content = append(result.Content, tools.TextContent(text))
			} else {
				if item.Result.Outcome == transcript.OutcomeCompleted {
					succeeded++
				} else {
					failed++
				}
				result.Parts = append(result.Parts, item.Result.Parts...)
				update, err := loop.ToolResult(transcript.ToolResult{Outcome: item.Result.Outcome, Parts: item.Result.Parts}, item.Result.Content)
				if err != nil {
					return tools.Result{}, err
				}
				result.Content = append(result.Content, update.Content...)
				if !item.Started {
					result.Parts = append(result.Parts, transcript.Part{Text: "The call was cancelled before it started."})
					result.Content = append(result.Content, tools.TextContent("The call was cancelled before it started."))
				}
				result.Parts = append(result.Parts, transcript.Part{Text: "\n\n"})
				result.Content = append(result.Content, tools.TextContent("\n\n"))
			}
		}
		footer := fmt.Sprintf("Completed: %d succeeded, %d failed", succeeded, failed)
		result.Parts = append(result.Parts, transcript.Part{Text: footer})
		result.Content = append(result.Content, tools.TextContent(footer))
		return result, nil
	}
	return t, nil
}
