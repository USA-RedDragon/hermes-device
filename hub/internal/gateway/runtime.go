package gateway

// MethodRuntimeDescribe reports what the backend serves: its capabilities
// name the verbs a client checks for before it calls them.
const MethodRuntimeDescribe = "runtime.describe"

// RuntimeDescription is the part of runtime.describe's result the hub
// reads.
type RuntimeDescription struct {
	Capabilities []string `json:"capabilities"`
}

// MethodBackgroundTool asks the running turn's tools to move to the
// background: a foreground terminal command hands its live process to the
// background process registry and the turn goes on. Hermes serves it only
// when runtime.describe lists CapToolBackground; v0.23.1 does not.
const MethodBackgroundTool = "session.background_tool"

// CapToolBackground is the runtime.describe capability of a Hermes that
// serves MethodBackgroundTool.
const CapToolBackground = "tool-background-v1"

// BackgroundToolParams is session.background_tool's params. Without
// ToolCallID every tool of the running turn is asked.
type BackgroundToolParams struct {
	SessionID           string `json:"session_id"`
	ExecutionGeneration int64  `json:"execution_generation"`
	ToolCallID          string `json:"tool_call_id,omitempty"`
}

// Statuses of BackgroundToolResult.
const (
	BackgroundToolRequested = "requested"
	BackgroundToolNone      = "none"
)

// BackgroundToolResult is session.background_tool's result: "requested"
// when Count tools were asked to move, "none" when no tool of the turn
// runs.
type BackgroundToolResult struct {
	Status string `json:"status"`
	Count  int    `json:"count"`
}
