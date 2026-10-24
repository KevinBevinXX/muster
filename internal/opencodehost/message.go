package opencodehost

import "encoding/json"

// Message is one entry of a session's messages (Session.Message.Info, a union
// keyed by Type), as much of it as muster reads. The union's members share
// their field names, so one struct holds every one of them.
type Message struct {
	ID   string `json:"id"`
	Type string `json:"type"`
	Time struct {
		Created   float64 `json:"created"`
		Completed float64 `json:"completed,omitempty"`
	} `json:"time"`
	// Text: user, synthetic, system, skill.
	Text string `json:"text,omitempty"`
	// Description: synthetic, system.
	Description string `json:"description,omitempty"`
	// Content, Finish and Error: assistant (Error also idle).
	Content []Content       `json:"content,omitempty"`
	Finish  string          `json:"finish,omitempty"`
	Error   json.RawMessage `json:"error,omitempty"`
	// Outcome: idle (succeeded, failed, interrupted).
	Outcome string `json:"outcome,omitempty"`
	// Name: skill. Command, Status: shell.
	Name    string `json:"name,omitempty"`
	Command string `json:"command,omitempty"`
	Status  string `json:"status,omitempty"`
}

// Content is one item of an assistant message's content: text, reasoning
// or tool.
type Content struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
	// ID and Name: a tool call's.
	ID    string    `json:"id,omitempty"`
	Name  string    `json:"name,omitempty"`
	State ToolState `json:"state,omitempty"`
}

// ToolState is a tool call's state: streaming (Input is the raw text so far),
// running, completed or error (Input an object).
type ToolState struct {
	Status string          `json:"status,omitempty"`
	Input  json.RawMessage `json:"input,omitempty"`
}
