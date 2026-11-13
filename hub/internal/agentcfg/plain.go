package agentcfg

import (
	"regexp"
	"strings"
	"unicode"
)

// plainToolsets is the one place a toolset id becomes words a person reads:
// a short label and a one-line description. Hermes names its toolsets with
// ids and describes them with tool ids ("web_search, web_extract"); the UI
// shows these instead. A toolset not listed here (a plugin's, a new
// upstream one) gets Humanize(id) and no description of its own.
var plainToolsets = map[string][2]string{
	"web":            {"Web search", "Search the web and read pages"},
	"browser":        {"Web browser", "Open pages, click, type and scroll in a browser"},
	"terminal":       {"Commands", "Run commands and manage background jobs"},
	"file":           {"Files", "Read, write, edit and search files"},
	"code_execution": {"Scripts", "Run short scripts"},
	"vision":         {"Image understanding", "Look at images and describe them"},
	"video":          {"Video understanding", "Watch videos and describe them (needs a model that can)"},
	"image_gen":      {"Image creation", "Make images from a description"},
	"video_gen":      {"Video creation", "Make short videos from text or images"},
	"x_search":       {"X search", "Search posts on X"},
	"tts":            {"Speech", "Read text aloud"},
	"stt":            {"Voice input", "Turn voice messages into text"},
	"skills":         {"Skills", "Find, read and manage skills"},
	"todo":           {"To-do list", "Plan a task and tick off the steps"},
	"kanban":         {"Task board", "Keep a board of tasks"},
	"memory":         {"Memory", "Remember things between chats"},
	"context_engine": {"Long chats", "Tools that help keep long chats going"},
	"session_search": {"Past chats", "Search earlier conversations"},
	"connections":    {"Accounts", "Use connected services and sign in to them"},
	"clarify":        {"Questions", "Ask you a question when something is unclear"},
	"delegation":     {"Helpers", "Hand parts of a task to helper agents"},
	"cronjob":        {"Schedules", "Run tasks on a schedule"},
	"discord":        {"Discord", "Read and take part in Discord chats"},
	"discord_admin":  {"Discord admin", "Manage a Discord server: channels, roles and pins"},
	"computer_use":   {"Computer control", "Use a desktop with mouse and keyboard"},
	"spotify":        {"Spotify", "Play and manage music on Spotify"},
	"a2a":            {"Other agents", "Talk to other agents"},
	"homeassistant":  {"Smart home", "Control smart home devices"},
	"rl":             {"Training", "Run training jobs"},
}

var (
	idToken = regexp.MustCompile(`[A-Za-z0-9]+(?:[_.][A-Za-z0-9]+)+`)
	acronym = map[string]string{"api": "API", "mcp": "MCP", "url": "URL", "oauth": "OAuth", "ssh": "SSH", "ai": "AI", "ascii": "ASCII", "svg": "SVG", "html": "HTML", "pcb": "PCB", "rf": "RF", "id": "ID", "tts": "TTS", "stt": "STT", "pdf": "PDF", "ui": "UI", "cli": "CLI", "gpt": "GPT", "llm": "LLM", "sql": "SQL", "kicad": "KiCad", "github": "GitHub", "openai": "OpenAI", "ios": "iOS", "macos": "macOS"}
)

// Humanize turns an id (web_search, systematic-debugging, mcp_servers,
// agent.locked_keys) into words: "Web search". The first word is
// capitalized, known acronyms keep their form, and text that already has
// spaces or capitals is left as it is.
func Humanize(id string) string {
	id = strings.TrimSpace(id)
	if id == "" {
		return ""
	}
	if strings.ContainsRune(id, ' ') || strings.ContainsFunc(id, unicode.IsUpper) {
		return id
	}
	words := strings.FieldsFunc(id, func(r rune) bool { return r == '_' || r == '-' || r == '.' || r == ':' || r == '/' })
	for i, w := range words {
		if a, ok := acronym[w]; ok {
			words[i] = a
		}
	}
	if len(words) > 0 && words[0] == strings.ToLower(words[0]) {
		r := []rune(words[0])
		r[0] = unicode.ToUpper(r[0])
		words[0] = string(r)
	}
	return strings.Join(words, " ")
}

// plainText keeps an upstream sentence only when no id shows in it
// (snake_case or dotted keys); otherwise it is dropped.
func plainText(s string) string {
	if idToken.MatchString(s) {
		return ""
	}
	return strings.TrimSpace(s)
}

// plainToolset fills the label and description of a toolset.
func plainToolset(t Toolset) Toolset {
	if p, ok := plainToolsets[t.Name]; ok {
		t.Label, t.Description = p[0], p[1]
		return t
	}
	t.Label = Humanize(strings.TrimPrefix(t.Name, "device-"))
	if l := plainText(t.Label); l != "" {
		t.Label = l
	}
	t.Description = plainText(t.Description)
	return t
}
