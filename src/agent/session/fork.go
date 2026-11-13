package session

import (
	"context"
	"encoding/json/v2"
	"errors"
	"time"
	"uuid"

	"github.com/gfx-labs/towa/pkg/acp"
)

// UnstableArchiveSession closes a session on any connection before hiding it
// from default list results. Its history stays in the store.
func (a *Agent) UnstableArchiveSession(ctx context.Context, p acp.UnstableArchiveSessionRequest) (acp.UnstableArchiveSessionResponse, error) {
	if a.store == nil {
		return acp.UnstableArchiveSessionResponse{}, acp.NewMethodNotFound("session/archive")
	}
	return acp.UnstableArchiveSessionResponse{}, a.archive(ctx, acp.SessionId(p.SessionId), true)
}

// UnstableUnarchiveSession unhides a stored session without attaching it.
func (a *Agent) UnstableUnarchiveSession(ctx context.Context, p acp.UnstableUnarchiveSessionRequest) (acp.UnstableUnarchiveSessionResponse, error) {
	if a.store == nil {
		return acp.UnstableUnarchiveSessionResponse{}, acp.NewMethodNotFound("session/unarchive")
	}
	return acp.UnstableUnarchiveSessionResponse{}, a.archive(ctx, acp.SessionId(p.SessionId), false)
}

func (a *Agent) archive(ctx context.Context, id acp.SessionId, archived bool) error {
	if err := a.ready(); err != nil {
		return err
	}
	defer a.lock(id)()
	if archived {
		if _, err := a.drain(ctx, id, nil); err != nil {
			return err
		}
	}
	// A loaded archived session can change config concurrently. Hold the
	// same lock from the metadata read through both durable and live writes.
	live, _ := a.lookup(id)
	if live != nil {
		live.setMu.Lock()
		defer live.setMu.Unlock()
	}
	meta, err := a.storedMeta(ctx, id)
	if err != nil {
		return err
	}
	meta.Archived = archived
	if err := a.store.SetMeta(ctx, meta); err != nil {
		return storeError(id, err)
	}
	// Unarchiving does not close a session that was explicitly loaded while
	// archived. Keep its next metadata write from restoring the old flag.
	if live != nil {
		live.mu.Lock()
		live.meta.Archived = archived
		live.mu.Unlock()
	}
	return nil
}

// UnstableForkSession snapshots the source's whole stored history into an
// independent session and attaches the new session to this connection.
func (a *Agent) UnstableForkSession(ctx context.Context, request acp.UnstableForkSessionRequest) (acp.UnstableForkSessionResponse, error) {
	// The generator emits stable and unstable copies of these nested
	// types. Convert using the protocol representation, not unsafe casts.
	var p acp.LoadSessionRequest
	if err := wireCopy(request, &p); err != nil {
		return acp.UnstableForkSessionResponse{}, err
	}
	if a.store == nil {
		return acp.UnstableForkSessionResponse{}, acp.NewMethodNotFound("session/fork")
	}
	if err := a.ready(); err != nil {
		return acp.UnstableForkSessionResponse{}, err
	}
	if err := errors.Join(a.checkDirs(p.Cwd, p.AdditionalDirectories), a.checkSystemPrompt(p.SystemPrompt), a.checkMCP(p.McpServers)); err != nil {
		var refused *acp.RequestError
		if errors.As(err, &refused) {
			return acp.UnstableForkSessionResponse{}, refused
		}
		return acp.UnstableForkSessionResponse{}, err
	}
	defer a.lock(p.SessionId)()
	meta, err := a.storedMeta(ctx, p.SessionId)
	if err != nil {
		return acp.UnstableForkSessionResponse{}, err
	}
	state, merged, prompt, err := a.restore(meta, p.Meta, p.SystemPrompt)
	if err != nil {
		return acp.UnstableForkSessionResponse{}, err
	}
	values, perTurn, err := a.startValues(merged, nil, state.Values)
	if err != nil {
		return acp.UnstableForkSessionResponse{}, err
	}
	repaired, err := a.repair(ctx, p.SessionId, meta.ID)
	if err != nil {
		return acp.UnstableForkSessionResponse{}, err
	}
	source := meta.ID
	id := uuid.NewV7()
	s := newSession(acp.SessionId(id.String()), p.Cwd)
	s.dirs, s.systemPrompt, s.mcpServers = p.AdditionalDirectories, prompt, p.McpServers
	s.values, s.perTurn = values, perTurn
	s.overrides = overrides(merged)
	s.title = meta.Title
	attached := false
	defer a.discard(ctx, s, &attached)
	a.announceAfterResponse(ctx, s)
	if err := a.openRuntime(ctx, s); err != nil {
		return acp.UnstableForkSessionResponse{}, err
	}
	meta.ID, meta.ParentID = id, uuid.Nil()
	meta.Cwd, meta.Created, meta.Updated, meta.Archived = p.Cwd, time.Now(), time.Now(), false
	if err := a.fillMeta(s, &meta); err != nil {
		return acp.UnstableForkSessionResponse{}, err
	}
	s.meta = meta
	if err := a.store.Fork(ctx, source, meta); err != nil {
		return acp.UnstableForkSessionResponse{}, storeError(p.SessionId, err)
	}
	if err := a.put(s, a); err != nil {
		return acp.UnstableForkSessionResponse{}, err
	}
	attached = true
	var response acp.UnstableForkSessionResponse
	err = wireCopy(acp.NewSessionResponse{SessionId: s.id, Modes: modeState(a.registry.option(ModeOption), values[ModeOption].ID), ConfigOptions: a.registry.configOptions(values, a.Gate().BooleanOptions()), Meta: repairMeta(repaired)}, &response)
	return response, err
}

func wireCopy(from, to any) error {
	encoded, err := json.Marshal(from)
	if err != nil {
		return err
	}
	return json.Unmarshal(encoded, to)
}
