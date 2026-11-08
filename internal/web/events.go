package web

import (
	"errors"
	"net/http"

	"github.com/gravexa-llc/ai-manager/internal/events"
	"github.com/gravexa-llc/ai-manager/internal/web/api"
)

// EventSink takes the mod's event batches from POST /events (internal/live's
// Receiver).
type EventSink interface {
	// Accept takes a decoded batch. It must not block.
	Accept(b events.Batch)
	// Refuse counts a POST refused as a whole.
	Refuse(reason string)
	// Stats returns the counters /api/status shows.
	Stats() api.EventStats
}

// WaitSource says what a session waits on (internal/live's Receiver): the
// permission, question or plan approval of its oldest
// pending ask, from the mod's needs_you events. ok is false when there is
// nothing to show. It must not block.
type WaitSource interface {
	WaitingOn(sessionID string) (w api.WaitingOn, ok bool)
}

// postEvents is POST /events: a batch of the mod's events
// ({"events": [...]}, internal/events). RequireToken has already checked
// X-AIM-Token. The answer is 200 {} as soon as the batch is decoded and
// queued: it never carries a decision, and events of unknown kinds or
// missing fields are dropped and counted rather than refused. Only a body
// over MaxBatchBytes or MaxBatchEvents (413) or one that isn't a batch at
// all (400) is refused, and the mod drops such a batch rather than
// resending it.
func (s *Server) postEvents(w http.ResponseWriter, r *http.Request) {
	body := http.MaxBytesReader(w, r.Body, events.MaxBatchBytes)
	b, err := events.DecodeBatch(body)
	if err != nil {
		var tooBig *http.MaxBytesError
		status, msg := http.StatusBadRequest, `body must be {"events": [...]}`
		switch {
		case errors.As(err, &tooBig):
			status, msg = http.StatusRequestEntityTooLarge, "batch larger than 256 KB"
		case errors.Is(err, events.ErrTooManyEvents):
			status, msg = http.StatusRequestEntityTooLarge, "more than 200 events in one batch"
		}
		if s.opts.Events != nil {
			s.opts.Events.Refuse(msg)
		}
		writeError(w, status, msg)
		return
	}
	if s.opts.Events != nil {
		s.opts.Events.Accept(b)
	}
	writeJSON(w, http.StatusOK, struct{}{})
}
