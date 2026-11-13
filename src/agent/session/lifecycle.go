package session

import (
	"context"
	"strings"

	acp "github.com/gfx-labs/go-acp"
)

// CloseSession cancels the session's turn, waits for it to stop and forgets
// the session. Closing a session that does not exist succeeds.
func (a *Agent) CloseSession(ctx context.Context, p acp.CloseSessionRequest) (acp.CloseSessionResponse, error) {
	if err := a.ready(); err != nil {
		return acp.CloseSessionResponse{}, err
	}
	// A session that is live on another connection is not this one's to
	// close, and is unknown to it like any session it has not loaded.
	defer a.lock(p.SessionId)()
	s, err := a.drain(ctx, p.SessionId, a)
	if err != nil {
		return acp.CloseSessionResponse{}, err
	}
	if s == nil {
		return acp.CloseSessionResponse{}, nil
	}
	a.logger.Info("session closed", "session", s.id)
	return acp.CloseSessionResponse{}, nil
}

// UnstableSessionStatus says whether the session is in memory, on any
// connection, or only in the store. It changes nothing.
func (a *Agent) UnstableSessionStatus(ctx context.Context, p acp.UnstableSessionStatusRequest) (acp.UnstableSessionStatusResponse, error) {
	if err := a.ready(); err != nil {
		return acp.UnstableSessionStatusResponse{}, err
	}
	id := acp.SessionId(p.SessionId)
	if s, _ := a.lookup(id); s != nil {
		return acp.UnstableSessionStatusResponse{Status: acp.UnstableSessionStatusOpen}, nil
	}
	if a.store == nil {
		return acp.UnstableSessionStatusResponse{}, unknownSession(id)
	}
	if _, err := a.storedMeta(ctx, id); err != nil {
		return acp.UnstableSessionStatusResponse{}, err
	}
	return acp.UnstableSessionStatusResponse{Status: acp.UnstableSessionStatusSaved}, nil
}

// UnstableSetSessionTitle sets a session's title. The session need not be
// live on this connection: one that is live on another has that
// connection's client told, and one that is only in the store has the
// title written there. With a store the title is written before the answer.
func (a *Agent) UnstableSetSessionTitle(ctx context.Context, p acp.UnstableSetSessionTitleRequest) (acp.UnstableSetSessionTitleResponse, error) {
	if err := a.ready(); err != nil {
		return acp.UnstableSetSessionTitleResponse{}, err
	}
	id := acp.SessionId(p.SessionId)
	title := strings.TrimSpace(p.Title)
	// Held so that the session is not loaded on a connection between the
	// lookup and the write, which would then keep the old title.
	defer a.lock(id)()
	s, owner := a.lookup(id)
	if s == nil {
		if a.store == nil {
			return acp.UnstableSetSessionTitleResponse{}, unknownSession(id)
		}
		meta, err := a.storedMeta(ctx, id)
		switch {
		case err != nil:
			return acp.UnstableSetSessionTitleResponse{}, err
		case title == "":
			return acp.UnstableSetSessionTitleResponse{}, invalid("title is empty")
		}
		meta.Title = title
		if err := a.store.SetMeta(ctx, meta); err != nil {
			return acp.UnstableSetSessionTitleResponse{}, storeError(id, err)
		}
		return acp.UnstableSetSessionTitleResponse{Title: title}, nil
	}
	if title == "" {
		return acp.UnstableSetSessionTitleResponse{}, invalid("title is empty")
	}
	if err := owner.announce(ctx, s); err != nil {
		return acp.UnstableSetSessionTitleResponse{}, err
	}
	s.setMu.Lock()
	defer s.setMu.Unlock()
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return acp.UnstableSetSessionTitleResponse{}, unknownSession(id)
	}
	s.title = title
	s.mu.Unlock()
	if err := a.persist(ctx, s); err != nil {
		return acp.UnstableSetSessionTitleResponse{}, err
	}
	err := owner.send(ctx, s, acp.SessionUpdate{SessionInfoUpdate: &acp.SessionSessionInfoUpdate{
		SessionUpdate: "session_info_update", Title: &title,
	}})
	return acp.UnstableSetSessionTitleResponse{Title: title}, err
}
