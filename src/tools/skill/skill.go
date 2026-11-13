// Package skill loads one skill by name for a model that asked for it.
package skill

import (
	"context"
	"errors"
	"fmt"

	"github.com/gfx-labs/towa/src/prompt/agentctx"
)

// Input names the skill to load.
type Input struct {
	Name string
}

// Result is a loaded skill.
type Result struct {
	Name        string
	Description string
	// Text is the skill's SKILL.md after its front matter.
	Text string
	// Dir is the skill's directory, an absolute path. Relative paths in
	// Text are relative to it, so the model can read the files beside it.
	Dir string
	// Path is the SKILL.md that was read.
	Path string
}

// Kind says why a skill was not loaded.
type Kind string

// Kinds of an Error.
const (
	// KindEmptyName: the call names no skill.
	KindEmptyName Kind = "empty_name"
	// KindUnknown: no listed skill has the name. Available holds the names
	// that exist.
	KindUnknown Kind = "unknown"
	// KindTooLarge: the skill's file is Size bytes and Limit is the most.
	KindTooLarge Kind = "too_large"
)

// Error is a call the model can correct. Any other error means the skill's
// file could not be read or has no usable front matter.
type Error struct {
	Kind Kind
	Name string
	// Available are the names of the listed skills, in listing order.
	Available []string
	// Size and Limit are in bytes.
	Size  int64
	Limit int64
}

func (e *Error) Error() string { return fmt.Sprintf("skill: %s: %q", e.Kind, e.Name) }

// Load reads the named skill from the session's listed skills, which the
// caller got from agentctx.ListSkills or agentctx.Load. The file is read
// again on every call, so an edit to a skill shows without a new session.
// maxBytes is the largest SKILL.md accepted, and zero means no limit: the
// caller passes context.max_file_bytes.
func Load(ctx context.Context, skills []agentctx.Skill, maxBytes int64, in Input) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	if in.Name == "" {
		return Result{}, &Error{Kind: KindEmptyName, Available: names(skills)}
	}
	loaded, err := agentctx.LoadSkill(skills, in.Name, maxBytes)
	var unknown *agentctx.UnknownSkillError
	var limit *agentctx.LimitError
	switch {
	case errors.As(err, &unknown):
		return Result{}, &Error{Kind: KindUnknown, Name: in.Name, Available: names(skills)}
	case errors.As(err, &limit):
		return Result{}, &Error{Kind: KindTooLarge, Name: in.Name, Size: limit.Size, Limit: limit.Limit}
	case err != nil:
		return Result{}, fmt.Errorf("skill: %w", err)
	}
	return Result{
		Name:        loaded.Name,
		Description: loaded.Description,
		Text:        loaded.Body,
		Dir:         loaded.Dir,
		Path:        loaded.Path,
	}, nil
}

func names(skills []agentctx.Skill) []string {
	out := make([]string, len(skills))
	for i, s := range skills {
		out[i] = s.Name
	}
	return out
}
