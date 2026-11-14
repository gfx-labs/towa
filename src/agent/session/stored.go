package session

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"slices"
	"time"
	"uuid"

	acp "github.com/gfx-labs/go-acp"
	"github.com/gfx-labs/towa/src/agent/harness"
	"github.com/gfx-labs/towa/src/agent/loop"
	"github.com/gfx-labs/towa/src/store"
)

// This file holds the methods that need the session store: load, resume,
// list and delete. Without a store each answers Method not found.

// attachRequest is what session/load and session/resume have in common.
type attachRequest struct {
	id           acp.SessionId
	cwd          string
	dirs         []string
	mcpServers   []acp.McpServer
	systemPrompt *acp.SystemPrompt
	initial      []acp.InitialSessionConfigOption
	meta         map[string]any
	repaired     *int64
}

// LoadSession brings a stored session into this process and replays its
// history to the client before it answers.
func (a *Agent) LoadSession(ctx context.Context, p acp.LoadSessionRequest) (acp.LoadSessionResponse, error) {
	if a.store == nil {
		return acp.LoadSessionResponse{}, acp.NewMethodNotFound(acp.AgentMethodSessionLoad)
	}
	var repaired int64
	modes, options, err := a.attach(ctx, attachRequest{
		id: p.SessionId, cwd: p.Cwd, dirs: p.AdditionalDirectories, mcpServers: p.McpServers,
		systemPrompt: p.SystemPrompt, initial: p.ConfigOptions, meta: p.Meta, repaired: &repaired,
	}, true)
	return acp.LoadSessionResponse{Modes: modes, ConfigOptions: options, Meta: repairMeta(repaired)}, err
}

// ResumeSession brings a stored session into this process without
// replaying its history.
func (a *Agent) ResumeSession(ctx context.Context, p acp.ResumeSessionRequest) (acp.ResumeSessionResponse, error) {
	if a.store == nil {
		return acp.ResumeSessionResponse{}, acp.NewMethodNotFound(acp.AgentMethodSessionResume)
	}
	var repaired int64
	modes, options, err := a.attach(ctx, attachRequest{
		id: p.SessionId, cwd: p.Cwd, dirs: p.AdditionalDirectories, mcpServers: p.McpServers,
		systemPrompt: p.SystemPrompt, initial: p.ConfigOptions, meta: p.Meta, repaired: &repaired,
	}, false)
	return acp.ResumeSessionResponse{Modes: modes, ConfigOptions: options, Meta: repairMeta(repaired)}, err
}

func repairMeta(bytes int64) map[string]any {
	if bytes == 0 {
		return nil
	}
	return map[string]any{harness.Key: map[string]any{"repair": map[string]any{"setAsideBytes": bytes}}}
}

// repair checks even a resume's tail, which does not replay history.
func (a *Agent) repair(ctx context.Context, id acp.SessionId, stored uuid.UUID) (int64, error) {
	n, err := a.store.Repair(ctx, stored)
	if err != nil {
		return 0, storeError(id, err)
	}
	if n > 0 {
		a.logger.WarnContext(ctx, "session torn tail set aside", "session", id, "set_aside_bytes", n)
		a.repairs.Add(ctx, 1)
	}
	return n, nil
}

// attach makes a stored session live in this process.
//
// A session that is already live in this process, on this connection or on
// another, is taken over and started again from the store: its running turn
// is cancelled and waited for, as session/close does, and the session is
// rebuilt from what the store holds and belongs to this connection.
// Config values, turn overrides and the system prompt survive in State.
// Tools allowed "always" are connection-local and are not restored.
func (a *Agent) attach(ctx context.Context, r attachRequest, replay bool) (*acp.SessionModeState, []acp.SessionConfigOption, error) {
	if err := a.ready(); err != nil {
		return nil, nil, err
	}
	if err := a.checkRequest(r.cwd, r.dirs, r.systemPrompt, r.mcpServers); err != nil {
		return nil, nil, err
	}
	// One at a time for a session, so that two loads cannot both make it
	// live.
	defer a.lock(r.id)()

	meta, err := a.storedMeta(ctx, r.id)
	if err != nil {
		return nil, nil, err
	}
	if meta.Cwd != r.cwd {
		return nil, nil, cwdMismatch(r.cwd, meta.Cwd)
	}
	plan, live, err := a.planAfterDrain(ctx, r, meta)
	if err != nil {
		return nil, nil, err
	}
	meta = plan.meta

	*r.repaired, err = a.repair(ctx, r.id, meta.ID)
	if err != nil {
		return nil, nil, err
	}
	s := newFromPlan(r.id, meta.Cwd, r.dirs, r.mcpServers, plan)
	s.meta = meta
	attached := false
	defer a.discard(ctx, s, &attached)
	a.announceAfterResponse(ctx, s)
	if err := a.openRuntime(ctx, s); err != nil {
		return nil, nil, err
	}
	if err := a.persist(ctx, s); err != nil {
		return nil, nil, err
	}
	if replay {
		if err := a.replayHistory(ctx, s); err != nil {
			return nil, nil, err
		}
	}
	if err := a.sendInfo(ctx, s, meta); err != nil {
		return nil, nil, err
	}
	if err := a.put(s, a); err != nil {
		return nil, nil, err
	}
	attached = true
	a.logger.Info("session attached from the store", "session", s.id, "replayed", replay, "was_live", live)
	return modeState(a.registry.option(ModeOption), plan.values[ModeOption].ID),
		a.registry.configOptions(plan.values, a.Gate().BooleanOptions()), nil
}

// attachPlan is what a stored session starts with in this process.
type attachPlan struct {
	meta    store.Meta
	prompt  *acp.SystemPrompt
	merged  map[string]any
	values  map[acp.SessionConfigId]Value
	perTurn []func(*loop.Turn)
}

// planAttach works out the state, system prompt and config values the
// request and the stored meta give the session.
func (a *Agent) planAttach(r attachRequest, meta store.Meta) (attachPlan, error) {
	state, merged, prompt, err := a.restore(meta, r.meta, r.systemPrompt)
	if err != nil {
		return attachPlan{}, err
	}
	values, perTurn, err := a.startValues(merged, r.initial, state.Values)
	if err != nil {
		return attachPlan{}, err
	}
	return attachPlan{meta: meta, prompt: prompt, merged: merged, values: values, perTurn: perTurn}, nil
}

// planAfterDrain plans the attach, then takes the session over from
// whichever connection has it. live says one had it, in which case the plan
// is made again from the metadata it left.
func (a *Agent) planAfterDrain(ctx context.Context, r attachRequest, meta store.Meta) (plan attachPlan, live bool, err error) {
	plan, err = a.planAttach(r, meta)
	if err != nil {
		return attachPlan{}, false, err
	}
	// From whichever connection has it. Its client is told nothing, since
	// the protocol has no message for it: the turn it was running answers
	// as cancelled, and its next request for the session answers that the
	// session is unknown.
	drained, err := a.drain(ctx, r.id, nil)
	if err != nil || drained == nil {
		return plan, false, err
	}
	// The old turn may have updated metadata while it stopped.
	meta, err = a.storedMeta(ctx, r.id)
	if err != nil {
		return attachPlan{}, true, err
	}
	plan, err = a.planAttach(r, meta)
	return plan, true, err
}

// newFromPlan makes a session that starts from a stored one, not yet open.
// The request's directory and server lists are the session's whole lists
// from now on.
func newFromPlan(id acp.SessionId, cwd string, dirs []string, servers []acp.McpServer, plan attachPlan) *session {
	s := newSession(id, cwd)
	s.dirs, s.systemPrompt, s.mcpServers = dirs, plan.prompt, servers
	s.values, s.perTurn = plan.values, plan.perTurn
	s.overrides = overrides(plan.merged)
	s.title = plan.meta.Title
	return s
}

// replayHistory sends the stored history of the session to the client.
func (a *Agent) replayHistory(ctx context.Context, s *session) error {
	var mapper loop.Mapper
	if s.runtime.Mapper != nil {
		mapper = *s.runtime.Mapper
	}
	// Every entry, including the ones a compaction took out of the
	// model's context: the client saw them.
	entries, err := a.store.History(ctx, s.meta.ID, store.Range{})
	if err != nil {
		return storeError(s.id, err)
	}
	var cost loop.Cost
	for entry, err := range entries {
		if err != nil {
			return storeError(s.id, err)
		}
		updates, err := mapper.Updates(entry.Item, &cost)
		if err != nil {
			return fmt.Errorf("session: replaying entry %s of session %s: %w", entry.ID, s.id, err)
		}
		for _, update := range updates {
			if err := a.send(ctx, s, update); err != nil {
				return err
			}
		}
	}
	return nil
}

// sendInfo tells the client what describes the session. The client named
// the session, so this can go out before the answer.
func (a *Agent) sendInfo(ctx context.Context, s *session, meta store.Meta) error {
	info := &acp.SessionSessionInfoUpdate{SessionUpdate: "session_info_update", UpdatedAt: new(meta.Updated.UTC().Format(time.RFC3339))}
	if meta.Title != "" {
		info.Title = &meta.Title
	}
	return a.send(ctx, s, acp.SessionUpdate{SessionInfoUpdate: info})
}

// ListSessions lists the stored sessions, oldest first. Sessions spawned
// as subagents are left out: a client learns of them from their parent.
func (a *Agent) ListSessions(ctx context.Context, p acp.ListSessionsRequest) (acp.ListSessionsResponse, error) {
	if a.store == nil {
		return acp.ListSessionsResponse{}, acp.NewMethodNotFound(acp.AgentMethodSessionList)
	}
	if err := a.ready(); err != nil {
		return acp.ListSessionsResponse{}, err
	}
	filter := store.Filter{Archived: store.ArchivedExclude}
	if p.Archived != nil {
		switch *p.Archived {
		case acp.SessionArchiveFilterExclude:
		case acp.SessionArchiveFilterInclude:
			filter.Archived = store.ArchivedAny
		case acp.SessionArchiveFilterOnly:
			filter.Archived = store.ArchivedOnly
		default:
			return acp.ListSessionsResponse{}, invalid("archived: unsupported filter %q", *p.Archived)
		}
	}
	if p.Cwd != nil {
		if err := a.checkDirs(*p.Cwd, nil); err != nil {
			return acp.ListSessionsResponse{}, err
		}
		filter.Cwd = *p.Cwd
	}
	// The cursor is the id of the last session of the page before. Ids
	// are UUIDv7 and the store lists them in order, so a session created
	// or deleted between two pages does not repeat or hide another.
	after := uuid.Nil()
	if p.Cursor != nil {
		id, err := uuid.Parse(*p.Cursor)
		if err != nil {
			return acp.ListSessionsResponse{}, invalid("cursor is not one this agent gave")
		}
		after = id
	}
	metas, err := a.store.List(ctx, filter)
	if err != nil {
		return acp.ListSessionsResponse{}, fmt.Errorf("session: listing sessions: %w", err)
	}
	// Not nil: the response requires the member.
	resp := acp.ListSessionsResponse{Sessions: []acp.SessionInfo{}}
	for _, meta := range metas {
		if meta.ParentID != uuid.Nil() || meta.ID.Compare(after) <= 0 {
			continue
		}
		if a.pageSize > 0 && len(resp.Sessions) == a.pageSize {
			resp.NextCursor = new(string(resp.Sessions[len(resp.Sessions)-1].SessionId))
			break
		}
		info := acp.SessionInfo{
			SessionId:             acp.SessionId(meta.ID.String()),
			Cwd:                   meta.Cwd,
			AdditionalDirectories: meta.Dirs,
			UpdatedAt:             new(meta.Updated.UTC().Format(time.RFC3339)),
			Archived:              new(meta.Archived),
		}
		if meta.Title != "" {
			info.Title = &meta.Title
		}
		resp.Sessions = append(resp.Sessions, info)
	}
	return resp, nil
}

// DeleteSession removes a session from the store. One that is live in this
// process, on any connection, is closed first: its turn is cancelled and
// waited for.
func (a *Agent) DeleteSession(ctx context.Context, p acp.DeleteSessionRequest) (acp.DeleteSessionResponse, error) {
	if a.store == nil {
		return acp.DeleteSessionResponse{}, acp.NewMethodNotFound(acp.AgentMethodSessionDelete)
	}
	if err := a.ready(); err != nil {
		return acp.DeleteSessionResponse{}, err
	}
	defer a.lock(p.SessionId)()
	meta, err := a.storedMeta(ctx, p.SessionId)
	if err != nil {
		return acp.DeleteSessionResponse{}, err
	}
	if _, err := a.drain(ctx, p.SessionId, nil); err != nil {
		return acp.DeleteSessionResponse{}, err
	}
	if err := a.store.Delete(ctx, meta.ID); err != nil {
		return acp.DeleteSessionResponse{}, storeError(p.SessionId, err)
	}
	a.logger.Info("session deleted", "session", p.SessionId)
	return acp.DeleteSessionResponse{}, nil
}

// storedMeta reads a session's Meta from the store. An id the store cannot
// hold is an unknown session.
func (a *Agent) storedMeta(ctx context.Context, id acp.SessionId) (store.Meta, error) {
	if err := a.ready(); err != nil {
		return store.Meta{}, err
	}
	parsed, err := uuid.Parse(string(id))
	if err != nil {
		return store.Meta{}, unknownSession(id)
	}
	meta, _, err := a.store.Load(ctx, parsed)
	if err != nil {
		return store.Meta{}, storeError(id, err)
	}
	return meta, nil
}

// storedValues returns the config option values kept in a session's Meta,
// for the options this process has.
func (a *Agent) storedValues(meta store.Meta) map[acp.SessionConfigId]Value {
	stored := map[acp.SessionConfigId]Value{}
	for id, value := range map[acp.SessionConfigId]string{ModeOption: meta.Mode, ModelOption: meta.Model} {
		if value != "" && a.registry.option(id) != nil {
			stored[id] = Value{ID: acp.SessionConfigValueId(value)}
		}
	}
	return stored
}

// fillMeta writes what the session has of the fields SetMeta replaces.
func (a *Agent) fillMeta(s *session, meta *store.Meta) error {
	meta.Title, meta.Dirs = s.title, s.dirs
	meta.Mode = string(s.values[ModeOption].ID)
	// Without a model option the stored model is not this process's to
	// change.
	if model, ok := s.values[ModelOption]; ok {
		meta.Model = string(model.ID)
	}
	// Select-only overrides have already become config values. Reapplying
	// them later would undo a subsequent set_config_option.
	turnOverrides := map[string]any{}
	for _, override := range a.registry.overrides {
		value, ok := s.overrides[override.Key]
		if !ok {
			continue
		}
		encoded, err := json.Marshal(value)
		if err != nil {
			return err
		}
		effect, err := override.Parse(encoded)
		if err != nil {
			return err
		}
		if effect.Turn != nil {
			turnOverrides[override.Key] = value
		}
	}
	state, err := json.Marshal(savedState{Values: s.values, Overrides: turnOverrides, SystemPrompt: s.systemPrompt}, json.Deterministic(true))
	if err != nil {
		return err
	}
	meta.State = state
	return nil
}

// persist writes the session's title, directories, mode and model to the
// store when they differ from what it has.
func (a *Agent) persist(ctx context.Context, s *session) error {
	if a.store == nil {
		return nil
	}
	s.mu.Lock()
	meta := s.meta
	err := a.fillMeta(s, &meta)
	same := meta.Title == s.meta.Title && meta.Mode == s.meta.Mode && meta.Model == s.meta.Model && slices.Equal(meta.Dirs, s.meta.Dirs) && bytes.Equal(meta.State, s.meta.State)
	s.mu.Unlock()
	if err != nil {
		return err
	}
	if same {
		return nil
	}
	if err := a.store.SetMeta(ctx, meta); err != nil {
		return storeError(s.id, err)
	}
	s.mu.Lock()
	s.meta = meta
	s.mu.Unlock()
	return nil
}

// storeError turns a store's error into the one the client gets.
func storeError(id acp.SessionId, err error) error {
	if errors.Is(err, store.ErrNotFound) {
		return unknownSession(id)
	}
	return fmt.Errorf("session: store, session %s: %w", id, err)
}

func cwdMismatch(asked, stored string) *acp.RequestError {
	return acp.NewInvalidParams(map[string]any{"error": "cwd is not the session's working directory", "cwd": asked, "sessionCwd": stored})
}
