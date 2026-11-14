package hub

import (
	"context"
	"encoding/json"
	"errors"
)

// ToolOutputs reads the full output of one tool row of a chat. A snapshot
// leaves the text of long tool outputs out (see web's trimToolRows); the
// page asks for it when a card opens.
type ToolOutputs interface {
	// ToolOutput returns the row of session sessionID that has row ID rowID
	// (when it is above 0) or tool call ID callID, as {"content": ...,
	// "text": ...}, each field only when the row has it. It fails with
	// ErrNoToolOutput when the session has no such tool row, and with the
	// errors of a session the user may not use.
	ToolOutput(ctx context.Context, user, backendID, sessionID string, rowID int64, callID string) (json.RawMessage, error)
}

// ErrNoToolOutput is a tool row the chat does not have.
var ErrNoToolOutput = errors.New("no such tool output in this chat")
