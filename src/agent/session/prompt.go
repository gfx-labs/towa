package session

import (
	"context"
	"errors"

	acp "github.com/gfx-labs/go-acp"
	"github.com/gfx-labs/towa/src/agent/harness"
	"github.com/gfx-labs/towa/src/agent/loop"
	"github.com/gfx-labs/towa/src/support/telemetry"
	"github.com/gfx-labs/towa/src/tools"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

// checkPrompt reports a content block of a type that is not advertised.
func (a *Agent) checkPrompt(prompt []acp.ContentBlock) error {
	for i, block := range prompt {
		switch {
		case block.Image != nil && !a.supports.Prompt.Image:
			return invalid("prompt[%d]: image content is not supported", i)
		case block.Audio != nil && !a.supports.Prompt.Audio:
			return invalid("prompt[%d]: audio content is not supported", i)
		case block.Resource != nil && !a.supports.Prompt.EmbeddedContext:
			return invalid("prompt[%d]: embedded resources are not supported", i)
		}
	}
	return nil
}

// Prompt runs one turn. A prompt for a session whose turn is still running
// cancels that turn and runs once it has stopped.
func (a *Agent) Prompt(ctx context.Context, p acp.PromptRequest) (acp.PromptResponse, error) {
	s, err := a.session(p.SessionId)
	if err != nil {
		return acp.PromptResponse{}, err
	}
	if err := a.checkPrompt(p.Prompt); err != nil {
		return acp.PromptResponse{}, err
	}
	arrival, ok := acp.Arrival(ctx)
	if !ok {
		return acp.PromptResponse{}, errors.New("session: prompt did not come through a connection")
	}
	if err := a.announce(ctx, s); err != nil {
		return acp.PromptResponse{}, err
	}

	ctx, span := a.tracer.Start(telemetry.WithRemoteParent(ctx, p.Meta), acp.AgentMethodSessionPrompt,
		trace.WithAttributes(attribute.String("acp.session.id", string(s.id))))
	defer span.End()

	result, err := s.run(ctx, arrival, func(ctx context.Context) (loop.Result, error) {
		return a.runTurn(ctx, s, arrival, p.Prompt)
	})
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		a.logger.ErrorContext(ctx, "turn failed", "session", s.id, "err", err)
		return acp.PromptResponse{}, stopError(err)
	}
	stop := attribute.String("acp.stop_reason", string(result.StopReason))
	span.SetAttributes(stop)
	a.turns.Add(ctx, 1, metric.WithAttributes(stop))
	resp := acp.PromptResponse{StopReason: result.StopReason, Usage: result.Usage}
	if result.Stop != nil {
		resp.Meta = harness.Meta(nil, harness.Addition{Stop: result.Stop})
	}
	return resp, nil
}

// runTurn builds the turn for a prompt and runs it, as a slash command or
// as a turn of the session's runtime.
func (a *Agent) runTurn(ctx context.Context, s *session, arrival uint64, prompt []acp.ContentBlock) (loop.Result, error) {
	gate := a.Gate()
	turn := loop.Turn{
		SessionID:    s.id,
		Cwd:          s.cwd,
		Prompt:       prompt,
		Dirs:         s.dirs,
		SystemPrompt: s.systemPrompt,
		McpServers:   s.mcpServers,
		Client:       gate.Client(),
		Sendable:     gate.Sendable,
		Update:       func(ctx context.Context, update acp.SessionUpdate) error { return a.send(ctx, s, update) },
		Permit: func(ctx context.Context, tool string, effect tools.Effect, call acp.ToolCallUpdate) (bool, error) {
			return a.permit(ctx, s, permitRequest{arrival: arrival, tool: tool, effect: effect, call: call})
		},
	}
	a.applyOptions(s, &turn)
	var result loop.Result
	var err error
	if command, args, ok := a.registry.command(prompt); ok && command.Run != nil {
		result, err = command.Run(ctx, CommandCall{
			Turn: turn, Args: args, Option: s.option,
			SetOption: func(ctx context.Context, id acp.SessionConfigId, value Value) error {
				option := a.registry.option(id)
				return a.setOption(ctx, s, optionChange{id: id, value: value, boolean: option != nil && option.Boolean})
			},
		})
	} else {
		result, err = s.runtime.Turn(ctx, turn)
	}
	if err != nil && ctx.Err() != nil {
		// run answers this with the cancelled stop reason. The error
		// is kept here in case it was not the cancellation's.
		a.logger.DebugContext(ctx, "cancelled turn returned an error", "session", s.id, "err", err)
	}
	return result, err
}

// applyOptions lets the session's config values and _meta overrides change
// the turn.
func (a *Agent) applyOptions(s *session, turn *loop.Turn) {
	for id, value := range s.options() {
		if apply := a.registry.option(id).Apply; apply != nil {
			apply(turn, value)
		}
	}
	for _, apply := range s.perTurn {
		apply(turn)
	}
}

// stopError turns a turn's failure into the error the client gets. A
// failure with a typed reason carries it in the error's data, so the client
// does not have to read the message.
func stopError(err error) error {
	// The loop reports a prompt a hook refused as its own error type. It is
	// carried to the client as a typed stop like the others.
	stopped, ok := errors.AsType[*loop.StopError](err)
	if denied, refused := errors.AsType[*loop.DeniedError](err); refused {
		stopped = &loop.StopError{Stop: harness.Stop{Kind: harness.StopDenied, Message: denied.Reason}}
	} else if !ok {
		return err
	}
	data := harness.Meta(map[string]any{"error": err.Error()}, harness.Addition{Stop: &stopped.Stop})
	// The turn did not start: the request was refused, the agent did not
	// fail.
	if stopped.Stop.Kind == harness.StopDenied {
		return acp.NewInvalidRequest(data)
	}
	if stopped.Stop.Kind == harness.StopAuth {
		return acp.NewAuthRequired(data)
	}
	return acp.NewInternalError(data)
}

// Cancel cancels the session's prompts that arrived before it.
func (a *Agent) Cancel(ctx context.Context, p acp.CancelNotification) error {
	s, err := a.session(p.SessionId)
	if err != nil {
		return err
	}
	arrival, ok := acp.Arrival(ctx)
	if !ok {
		return errors.New("session: cancel did not come through a connection")
	}
	s.cancel(arrival)
	return nil
}
