package extras

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/gfx-labs/towa/pkg/acp"
	"github.com/gfx-labs/towa/src/config"
	"github.com/gfx-labs/towa/src/prompt/agentctx"
	"github.com/gfx-labs/towa/src/tools"
	"github.com/gfx-labs/towa/src/tools/skill"
	"github.com/gfx-labs/towa/src/transcript"
)

// Skill loads one of the session's listed skills. An empty, non-nil list is
// allowed: its calls report that no named skill exists.
func Skill(cfg config.Context, skills []agentctx.Skill) (tools.Tool, error) {
	if !cfg.SkillsEnabled {
		return tools.Tool{}, fmt.Errorf("skill_manage: %w", ErrDisabled)
	}
	maxBytes := cfg.MaxFileBytes
	if skills == nil {
		return tools.Tool{}, errors.New("skill_manage: no skill listing")
	}
	if maxBytes < 0 {
		return tools.Tool{}, errors.New("skill_manage: negative file limit")
	}
	skills = slices.Clone(skills)
	t := newTool(NameSkill, "Load a skill listed in the system prompt, by name. Returns the skill's full instructions and the directory its files are in. Load a skill before starting a task that its description matches.", acp.ToolKindRead, []byte(`{"type":"object","required":["name"],"properties":{"intent":{"type":"string"},"action":{"type":"string","enum":["load"]},"name":{"type":"string","description":"The skill's name as the system prompt lists it."}}}`))
	t.Summary = "Load a skill's instructions by name"
	t.Run = func(ctx context.Context, call tools.Call) (tools.Result, error) {
		var in struct {
			Name   string `json:"name"`
			Action string `json:"action"`
		}
		if err := json.Unmarshal(call.Input, &in); err != nil {
			return tools.Result{}, err
		}
		if in.Action != "" && in.Action != "load" {
			return textResult(transcript.OutcomeFailed, "Only the load action is supported."), nil
		}
		name := strings.TrimLeft(strings.TrimSpace(in.Name), "/")
		res, err := skill.Load(ctx, skills, maxBytes, skill.Input{Name: name})
		if err != nil {
			var e *skill.Error
			if !errors.As(err, &e) {
				return tools.Result{}, err
			}
			text := e.Error()
			switch e.Kind {
			case skill.KindEmptyName:
				text = "'name' is required for load action"
			case skill.KindUnknown:
				text = fmt.Sprintf("Skill '%s' not found. Available skills: %s", e.Name, strings.Join(e.Available, ", "))
			case skill.KindTooLarge:
				text = fmt.Sprintf("Skill '%s' is %d bytes, over context.max_file_bytes (%d).", e.Name, e.Size, e.Limit)
			}
			return textResult(transcript.OutcomeFailed, text), nil
		}
		return textResult(transcript.OutcomeCompleted, fmt.Sprintf("## Skill: %s\n\n**Base directory**: %s\n\n%s", res.Name, res.Dir, res.Text)), nil
	}
	return t, nil
}
