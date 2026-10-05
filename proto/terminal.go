package proto

// TerminalOpen asks the agent to dial a terminal socket. Sent from v3.
type TerminalOpen struct {
	SessionID   string `json:"session_id"`
	AgentTicket string `json:"agent_ticket"`
	Cols        int    `json:"cols"`
	Rows        int    `json:"rows"`
}

// TerminalOpenResult is "ok" or "refused".
type TerminalOpenResult struct {
	SessionID string `json:"session_id"`
	Result    string `json:"result"`
}

// TerminalHello is the agent's first text frame on /ws/terminal.
type TerminalHello struct {
	SessionID   string `json:"session_id"`
	AgentTicket string `json:"agent_ticket"`
}

// TerminalControl is a JSON text frame on either terminal socket.
type TerminalControl struct {
	Type string `json:"type"` // "resize" or "exit"
	Cols int    `json:"cols,omitempty"`
	Rows int    `json:"rows,omitempty"`
	Code int    `json:"code,omitempty"`
}

// TerminalMaxFrame is the largest WebSocket message either end of
// /ws/terminal accepts. The server sets it on every terminal socket and the
// agent sets it on the socket it dials; the browser chunks input well below it.
const TerminalMaxFrame = 64 << 10
