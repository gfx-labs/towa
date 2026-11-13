package extras

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"

	acp "github.com/gfx-labs/go-acp"

	"github.com/gfx-labs/towa/src/agent/loop"
	"github.com/gfx-labs/towa/src/config"
	"github.com/gfx-labs/towa/src/tools"
	"github.com/gfx-labs/towa/src/tools/batch"
	"github.com/gfx-labs/towa/src/transcript"
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
		"tool_calls": map[string]any{"type": "array", "minItems": 1, "maxItems": cfg.MaxCalls, "items": map[string]any{"type": "object", "required": []string{"tool"}, "properties": map[string]any{"tool": map[string]any{"type": "string"}}, "additionalProperties": true}},
	}})
	if err != nil {
		return tools.Tool{}, err
	}
	b := batchTool{cfg: cfg, dispatcher: dispatcher}
	t := newTool(NameBatch, "Run several independent tool calls in one call and return every result. Each entry contains tool and that tool's own arguments as sibling fields, and nothing else. Calls that only read run at the same time, and a call that writes or executes runs in the order given. Do not batch calls that depend on each other's results, and do not nest batch.", acp.ToolKindExecute, schema)
	t.Summary = "Run several independent tool calls in one call"
	// Children can execute. Serialize the parent against outer calls as well.
	t.Effect = tools.Executes
	t.Run = b.run
	return t, nil
}

// batchTool holds what one call needs besides its input.
type batchTool struct {
	cfg        config.ToolsBatch
	dispatcher func(context.Context) (batch.Dispatcher, bool)
}

var errNestedBatch = errors.New("batch: nested batch")

func (b batchTool) run(ctx context.Context, call tools.Call) (tools.Result, error) {
	calls, err := decodeBatchCalls(call.Input)
	if errors.Is(err, errNestedBatch) {
		return tools.TextResult(transcript.OutcomeFailed, "Cannot batch the 'batch' tool"), nil
	}
	if err != nil {
		return tools.Result{}, err
	}
	d, ok := b.dispatcher(ctx)
	if !ok || d.Plan == nil || d.Run == nil {
		return tools.Result{}, errors.New("batch: no dispatcher for this call")
	}
	items, err := batch.Run(ctx, b.cfg, d, calls)
	if err != nil {
		e, ok := errors.AsType[*batch.Error](err)
		if !ok {
			return tools.Result{}, err
		}
		text := "No tool calls provided"
		if e.Kind == batch.KindTooMany {
			text = fmt.Sprintf("Maximum %d tool calls allowed", e.Limit)
		}
		return tools.TextResult(transcript.OutcomeFailed, text), nil
	}
	return formatBatch(calls, items)
}

// decodeBatchCalls splits each entry into the tool name and that tool's own input.
func decodeBatchCalls(input jsontext.Value) ([]batch.Call, error) {
	var in struct {
		Calls []map[string]jsontext.Value `json:"tool_calls"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return nil, err
	}
	calls := make([]batch.Call, 0, len(in.Calls))
	for _, raw := range in.Calls {
		var name string
		if err := json.Unmarshal(raw["tool"], &name); err != nil {
			return nil, err
		}
		if name == NameBatch {
			return nil, errNestedBatch
		}
		delete(raw, "tool")
		childInput, err := json.Marshal(raw)
		if err != nil {
			return nil, err
		}
		calls = append(calls, batch.Call{Name: name, Input: childInput})
	}
	return calls, nil
}

// batchResult builds the model text and the client content in step.
type batchResult struct{ tools.Result }

func (r *batchResult) addText(text string) {
	r.Parts = append(r.Parts, transcript.Part{Text: text})
	r.Content = append(r.Content, tools.TextContent(text))
}

func formatBatch(calls []batch.Call, items []batch.Item) (tools.Result, error) {
	result := batchResult{tools.Result{Outcome: batch.Outcome(items)}}
	succeeded, failed := 0, 0
	for i, item := range items {
		result.addText(fmt.Sprintf("--- [%d] %s ---\n", i+1, calls[i].Name))
		if item.Err != nil {
			failed++
			result.addText(fmt.Sprintf("Error: %v\n\n", item.Err))
			continue
		}
		if item.Result.Outcome == transcript.OutcomeCompleted {
			succeeded++
		} else {
			failed++
		}
		if err := result.addChildResult(item); err != nil {
			return tools.Result{}, err
		}
	}
	result.addText(fmt.Sprintf("Completed: %d succeeded, %d failed", succeeded, failed))
	return result.Result, nil
}

func (r *batchResult) addChildResult(item batch.Item) error {
	r.Parts = append(r.Parts, item.Result.Parts...)
	update, err := loop.ToolResult(transcript.ToolResult{Outcome: item.Result.Outcome, Parts: item.Result.Parts}, item.Result.Content)
	if err != nil {
		return err
	}
	r.Content = append(r.Content, update.Content...)
	if !item.Started {
		r.addText("The call was cancelled before it started.")
	}
	r.addText("\n\n")
	return nil
}
