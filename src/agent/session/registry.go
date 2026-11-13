package session

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"slices"
	"strings"

	acp "github.com/gfx-labs/go-acp"
	"github.com/gfx-labs/towa/src/agent/harness"
	"github.com/gfx-labs/towa/src/agent/loop"
	"github.com/gfx-labs/towa/src/config"
)

// ModeOption is the id of the config option that holds the session's mode.
const ModeOption acp.SessionConfigId = "mode"

// The values of ModeOption, which are also the ids of the session modes.
const (
	ModeAuto acp.SessionConfigValueId = config.SessionModeAuto
	ModeAsk  acp.SessionConfigValueId = config.SessionModeAsk
)

// Value is the value of a config option: ID for a select option, On for a
// boolean one.
type Value struct {
	ID acp.SessionConfigValueId
	On bool
}

// Choice is one value a select option can take.
type Choice struct {
	Value       acp.SessionConfigValueId
	Name        string
	Description string
}

// ConfigOption is a per-session setting a client reads from session/new and
// changes with session/set_config_option.
type ConfigOption struct {
	ID          acp.SessionConfigId
	Name        string
	Description string
	// Category tells a client what kind of option this is. It may be empty.
	Category acp.SessionConfigOptionCategory
	// Boolean makes this an on/off option. It is listed and settable only
	// for a client that advertised boolean options, and keeps its default
	// for any other client.
	Boolean bool
	// Choices are the values of a select option, in display order.
	Choices []Choice
	// Default is the value a new session starts with.
	Default Value
	// Validate refuses a value that is one of the choices and still cannot
	// be used. It may be nil.
	Validate func(value Value) error
	// Apply writes the session's value into each turn before it runs. It is
	// nil for an option the session package acts on itself.
	Apply func(turn *loop.Turn, value Value)
}

// check reports a value the option cannot take.
func (o *ConfigOption) check(v Value) error {
	if o.Boolean {
		if v.ID != "" {
			return fmt.Errorf("config option %q is a boolean and has no value %q", o.ID, v.ID)
		}
		return nil
	}
	if !slices.ContainsFunc(o.Choices, func(c Choice) bool { return c.Value == v.ID }) {
		return fmt.Errorf("config option %q has no value %q", o.ID, v.ID)
	}
	if o.Validate != nil {
		return o.Validate(v)
	}
	return nil
}

// Command is a slash command the client is told about.
type Command struct {
	// Name is the command without its slash.
	Name        string
	Description string
	// InputHint describes the text after the name. Empty means the command
	// takes none.
	InputHint string
	// Run answers a prompt that starts with the command, in place of the
	// turn. When it is nil the prompt goes to the turn, which handles the
	// command itself.
	Run func(ctx context.Context, call CommandCall) (loop.Result, error)
}

// CommandCall is one use of a command.
type CommandCall struct {
	// Turn is the turn the prompt would have run as.
	Turn loop.Turn
	// Args is the text after the command's name, without surrounding space.
	Args string
	// Option returns the session's value of a config option.
	Option func(id acp.SessionConfigId) (Value, bool)
	// SetOption changes a config option of the session and tells the client.
	SetOption func(ctx context.Context, id acp.SessionConfigId, value Value) error
}

// Override is one key a client may set under _meta.towa on
// session/new. A key that no Override names is rejected.
type Override struct {
	Key string
	// Parse checks the key's JSON value and says what it changes.
	Parse func(value jsontext.Value) (OverrideEffect, error)
}

// OverrideEffect is what one override changes in the new session.
type OverrideEffect struct {
	// Set gives config options their starting value.
	Set map[acp.SessionConfigId]Value
	// Turn changes each turn of the session before it runs.
	Turn func(turn *loop.Turn)
}

// Registry holds what packages add to sessions: config options, slash
// commands and _meta.towa override keys. Fill it before NewAgent,
// which copies it.
type Registry struct {
	options   []ConfigOption
	commands  []Command
	overrides []Override
}

// AddConfigOption adds a config option. Options are listed to the client in
// the order they were added.
func (r *Registry) AddConfigOption(o ConfigOption) error {
	switch {
	case o.ID == "":
		return fmt.Errorf("session: config option has no id")
	case o.Name == "":
		return fmt.Errorf("session: config option %q has no name", o.ID)
	case r.option(o.ID) != nil:
		return fmt.Errorf("session: config option %q is registered twice", o.ID)
	case o.Boolean && len(o.Choices) > 0:
		return fmt.Errorf("session: config option %q is a boolean with choices", o.ID)
	case !o.Boolean && len(o.Choices) == 0:
		return fmt.Errorf("session: config option %q has no choices", o.ID)
	}
	if err := o.check(o.Default); err != nil {
		return fmt.Errorf("session: default of %w", err)
	}
	r.options = append(r.options, o)
	return nil
}

// AddCommand adds a slash command.
func (r *Registry) AddCommand(c Command) error {
	switch {
	case c.Name == "" || strings.HasPrefix(c.Name, "/") || strings.ContainsFunc(c.Name, isSpace):
		return fmt.Errorf("session: command name %q is empty, starts with a slash or has a space", c.Name)
	case c.Description == "":
		return fmt.Errorf("session: command %q has no description", c.Name)
	case slices.ContainsFunc(r.commands, func(have Command) bool { return have.Name == c.Name }):
		return fmt.Errorf("session: command %q is registered twice", c.Name)
	}
	r.commands = append(r.commands, c)
	return nil
}

// AddOverride allows a key under _meta.towa on session/new.
func (r *Registry) AddOverride(o Override) error {
	switch {
	case o.Key == "":
		return fmt.Errorf("session: override has no key")
	case o.Parse == nil:
		return fmt.Errorf("session: override %q has no Parse", o.Key)
	case slices.ContainsFunc(r.overrides, func(have Override) bool { return have.Key == o.Key }):
		return fmt.Errorf("session: override %q is registered twice", o.Key)
	}
	r.overrides = append(r.overrides, o)
	return nil
}

func (r *Registry) clone() *Registry {
	return &Registry{
		options:   slices.Clone(r.options),
		commands:  slices.Clone(r.commands),
		overrides: slices.Clone(r.overrides),
	}
}

func (r *Registry) option(id acp.SessionConfigId) *ConfigOption {
	for i := range r.options {
		if r.options[i].ID == id {
			return &r.options[i]
		}
	}
	return nil
}

// defaults returns the values a new session starts with.
func (r *Registry) defaults() map[acp.SessionConfigId]Value {
	values := make(map[acp.SessionConfigId]Value, len(r.options))
	for _, o := range r.options {
		values[o.ID] = o.Default
	}
	return values
}

// applyMeta applies the overrides in a session/new request's _meta to
// values and returns the changes they make to each turn. Other keys of
// _meta belong to other parties and are left alone.
func (r *Registry) applyMeta(meta map[string]any, values map[acp.SessionConfigId]Value) ([]func(*loop.Turn), error) {
	raw, ok := meta[harness.Key]
	if !ok {
		return nil, nil
	}
	sent, ok := raw.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("_meta.%s is not an object", harness.Key)
	}
	var perTurn []func(*loop.Turn)
	// In registration order, so that the outcome does not depend on the
	// order of a map.
	for _, o := range r.overrides {
		value, ok := sent[o.Key]
		if !ok {
			continue
		}
		at := "_meta." + harness.Key + "." + o.Key
		encoded, err := json.Marshal(value)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", at, err)
		}
		effect, err := o.Parse(encoded)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", at, err)
		}
		for id, v := range effect.Set {
			option := r.option(id)
			if option == nil {
				return nil, fmt.Errorf("%s: sets config option %q, which does not exist", at, id)
			}
			if err := option.check(v); err != nil {
				return nil, fmt.Errorf("%s: %w", at, err)
			}
			values[id] = v
		}
		if effect.Turn != nil {
			perTurn = append(perTurn, effect.Turn)
		}
	}
	for key := range sent {
		if !slices.ContainsFunc(r.overrides, func(o Override) bool { return o.Key == key }) {
			return nil, fmt.Errorf("_meta.%s.%s is not a key a session can override", harness.Key, key)
		}
	}
	return perTurn, nil
}

// command returns the command a prompt starts with and the text after it.
func (r *Registry) command(prompt []acp.ContentBlock) (Command, string, bool) {
	if len(prompt) == 0 || prompt[0].Text == nil {
		return Command{}, "", false
	}
	text, ok := strings.CutPrefix(prompt[0].Text.Text, "/")
	if !ok {
		return Command{}, "", false
	}
	name, args := text, ""
	if i := strings.IndexFunc(text, isSpace); i >= 0 {
		name, args = text[:i], strings.TrimSpace(text[i:])
	}
	for _, c := range r.commands {
		if c.Name == name {
			return c, args, true
		}
	}
	return Command{}, "", false
}

func isSpace(r rune) bool { return r == ' ' || r == '\t' || r == '\n' || r == '\r' }

// available returns the commands as the client is told about them.
func (r *Registry) available() []acp.AvailableCommand {
	commands := make([]acp.AvailableCommand, len(r.commands))
	for i, c := range r.commands {
		commands[i] = acp.AvailableCommand{Name: c.Name, Description: c.Description}
		if c.InputHint != "" {
			commands[i].Input = &acp.AvailableCommandInput{Unstructured: &acp.UnstructuredCommandInput{Hint: c.InputHint}}
		}
	}
	return commands
}

// listed returns the options a client is shown, in order.
func (r *Registry) listed(booleans bool) []*ConfigOption {
	var shown []*ConfigOption
	for i := range r.options {
		if !r.options[i].Boolean || booleans {
			shown = append(shown, &r.options[i])
		}
	}
	return shown
}

// configOptions returns the options with a session's values. Boolean
// options are left out for a client that did not advertise them.
func (r *Registry) configOptions(values map[acp.SessionConfigId]Value, booleans bool) []acp.SessionConfigOption {
	// Not nil: the set_config_option response requires the member.
	out := []acp.SessionConfigOption{}
	for _, o := range r.listed(booleans) {
		if o.Boolean {
			out = append(out, acp.SessionConfigOption{Boolean: &acp.SessionConfigOptionBoolean{
				Id: o.ID, Name: o.Name, Description: optional(o.Description), Category: category(o.Category),
				CurrentValue: values[o.ID].On, Type: "boolean",
			}})
			continue
		}
		choices := make(acp.SessionConfigSelectOptionsUngrouped, len(o.Choices))
		for i, c := range o.Choices {
			choices[i] = acp.SessionConfigSelectOption{Value: c.Value, Name: c.Name, Description: optional(c.Description)}
		}
		out = append(out, acp.SessionConfigOption{Select: &acp.SessionConfigOptionSelect{
			Id: o.ID, Name: o.Name, Description: optional(o.Description), Category: category(o.Category),
			CurrentValue: values[o.ID].ID, Options: acp.SessionConfigSelectOptions{Ungrouped: &choices}, Type: "select",
		}})
	}
	return out
}

// unstableConfigOptions is configOptions in the types of session/options,
// which the schema generates separately.
func (r *Registry) unstableConfigOptions(values map[acp.SessionConfigId]Value, booleans bool) []acp.UnstableSessionConfigOption {
	var out []acp.UnstableSessionConfigOption
	for _, o := range r.listed(booleans) {
		if o.Boolean {
			out = append(out, acp.UnstableSessionConfigOption{Boolean: &acp.UnstableSessionConfigOptionBoolean{
				Id: o.ID, Name: o.Name, Description: optional(o.Description), Category: category(o.Category),
				CurrentValue: values[o.ID].On, Type: "boolean",
			}})
			continue
		}
		choices := make(acp.UnstableSessionConfigSelectOptionsUngrouped, len(o.Choices))
		for i, c := range o.Choices {
			choices[i] = acp.UnstableSessionConfigSelectOption{Value: c.Value, Name: c.Name, Description: optional(c.Description)}
		}
		out = append(out, acp.UnstableSessionConfigOption{Select: &acp.UnstableSessionConfigOptionSelect{
			Id: o.ID, Name: o.Name, Description: optional(o.Description), Category: category(o.Category),
			CurrentValue: values[o.ID].ID, Options: acp.UnstableSessionConfigSelectOptions{Ungrouped: &choices}, Type: "select",
		}})
	}
	return out
}

func optional(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func category(c acp.SessionConfigOptionCategory) *acp.SessionConfigOptionCategory {
	if c == "" {
		return nil
	}
	return &c
}

// modeOption is the mode config option, starting in the configured mode.
func modeOption(start acp.SessionConfigValueId) ConfigOption {
	return ConfigOption{
		ID:          ModeOption,
		Name:        "Mode",
		Description: "Whether tool calls that write or execute need approval.",
		Category:    acp.SessionConfigOptionCategoryMode,
		Choices: []Choice{
			{Value: ModeAuto, Name: "Auto", Description: "Run every tool call without asking."},
			{Value: ModeAsk, Name: "Ask", Description: "Ask before a tool call that writes files or runs a command."},
		},
		Default: Value{ID: start},
	}
}

// modeOverride lets session/new choose the mode in _meta.towa.mode.
func modeOverride() Override {
	return Override{Key: string(ModeOption), Parse: func(value jsontext.Value) (OverrideEffect, error) {
		var mode string
		if err := json.Unmarshal(value, &mode); err != nil {
			return OverrideEffect{}, err
		}
		return OverrideEffect{Set: map[acp.SessionConfigId]Value{ModeOption: {ID: acp.SessionConfigValueId(mode)}}}, nil
	}}
}

// pathsOverride lets session/new name a list of paths under key. The paths
// are carried to the turn as sent: config.Context's rules for them apply
// where the context is loaded.
func pathsOverride(key string, set func(turn *loop.Turn, paths []string)) Override {
	return Override{Key: key, Parse: func(value jsontext.Value) (OverrideEffect, error) {
		var paths []string
		if err := json.Unmarshal(value, &paths); err != nil {
			return OverrideEffect{}, err
		}
		if slices.Contains(paths, "") {
			return OverrideEffect{}, errors.New("a path is empty")
		}
		return OverrideEffect{Turn: func(turn *loop.Turn) { set(turn, paths) }}, nil
	}}
}

// modeCommand is /mode: it reports the session's mode, or changes it.
func modeCommand() Command {
	return Command{
		Name:        string(ModeOption),
		Description: "Show the session's mode, or switch to the named one.",
		InputHint:   string(ModeAuto) + " or " + string(ModeAsk),
		Run: func(ctx context.Context, call CommandCall) (loop.Result, error) {
			if call.Args != "" {
				if err := call.SetOption(ctx, ModeOption, Value{ID: acp.SessionConfigValueId(call.Args)}); err != nil {
					return loop.Result{}, err
				}
			}
			mode, _ := call.Option(ModeOption)
			reply := acp.SessionUpdate{AgentMessageChunk: &acp.SessionUpdateAgentMessageChunk{
				Content: acp.ContentBlock{Text: &acp.ContentBlockText{Type: "text", Text: "Mode: " + string(mode.ID)}},
			}}
			if err := call.Turn.Update(ctx, reply); err != nil {
				return loop.Result{}, err
			}
			return loop.Result{StopReason: acp.StopReasonEndTurn}, nil
		},
	}
}
