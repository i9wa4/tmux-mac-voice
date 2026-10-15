package voice

import (
	"context"
	"errors"
	"time"
)

var ErrAdapterUnavailable = errors.New("Postman text adapter is not configured (#767)")

// TurnRequest is the text-only boundary to the authenticated local Postman
// adapter. A retry must reuse RequestID. The adapter must authenticate its
// external caller, durably deduplicate accepted requests, correlate the final
// result or timeout, and never report success before the result is durable.
type TurnRequest struct {
	SessionID string    `json:"session_id"`
	TurnID    string    `json:"turn_id"`
	RequestID string    `json:"request_id"`
	Target    string    `json:"target"`
	Text      string    `json:"text"`
	Deadline  time.Time `json:"deadline"`
}

type TurnResult struct {
	SessionID string `json:"session_id"`
	TurnID    string `json:"turn_id"`
	RequestID string `json:"request_id"`
	ResultID  string `json:"result_id"`
	Status    string `json:"status"`
	Text      string `json:"text"`
}

type TextAdapter interface {
	// Exchange returns only a terminal result. Accepted-but-pending is not a
	// spoken response and remains the adapter's responsibility.
	Exchange(context.Context, TurnRequest) (TurnResult, error)
}

type UnavailableAdapter struct{}

func (UnavailableAdapter) Exchange(context.Context, TurnRequest) (TurnResult, error) {
	return TurnResult{}, ErrAdapterUnavailable
}
