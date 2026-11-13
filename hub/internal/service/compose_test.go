package service_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/USA-RedDragon/hermes-hub/internal/gateway"
	"github.com/USA-RedDragon/hermes-hub/internal/gateway/gatewaytest"
	"github.com/USA-RedDragon/hermes-hub/internal/hub"
	"github.com/USA-RedDragon/hermes-hub/internal/service"
)

const (
	textSteer  = "also check the tests"
	textOrig   = "hi"
	roleAsst   = "assistant"
	titleCopy  = "Copy"
	cmdUsage   = "/usage"
	sessionNew = "s-new"
	textEdit   = "edited"
)

func fastSteer() service.Options {
	o := fastHold()
	o.SteerRetry, o.SteerWindow = 10*time.Millisecond, 2*time.Second
	return o
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func calls(e *env, method string) int {
	n := 0
	for _, c := range e.gw.Calls() {
		if c.Method == method {
			n++
		}
	}
	return n
}

// running starts a turn on sLive that another client sent, and lets the
// hub see it.
func running(t *testing.T, e *env) {
	t.Helper()
	e.gw.QueueWhileRunning = true
	e.gw.StartElsewhere(sLive, "tui-1", "work", false, true)
}

func TestSessionsActivityTimeAndNeeds(t *testing.T) {
	t.Parallel()
	e := setup(t, service.Options{})
	ctx := context.Background()
	e.gw.AddSession(&gatewaytest.Session{ID: "s-active", Title: "Active", Live: true, StartedAt: 1500, LastActiveAt: 5000.25})
	e.gw.AddSession(&gatewaytest.Session{ID: "s-ask", Title: "Asks", Live: true, StartedAt: 1200, Running: true,
		Prompts: []gateway.Prompt{{Kind: gateway.PromptApproval, PromptID: "ap-9", Choices: []string{gateway.ChoiceOnce}}}})
	e.gw.AddSession(&gatewaytest.Session{ID: "s-q", Title: "Question", Live: true, StartedAt: 1100, Running: true,
		Prompts: []gateway.Prompt{{Kind: gateway.PromptClarify, PromptID: "cl-9"}}})
	ss, err := e.svc.Sessions(ctx, userAlice)
	if err != nil {
		t.Fatal(err)
	}
	if ss[0].ID != "s-active" || ss[0].UpdatedAt.UnixMilli() != 5000250 {
		t.Fatalf("first = %+v, want s-active at its last activity", ss[0])
	}
	for _, s := range ss {
		if s.Needs != "" {
			t.Fatalf("%s needs %q before the hub watched it", s.ID, s.Needs)
		}
	}
	for _, id := range []string{"s-ask", "s-q"} {
		ch, stop, err := e.svc.Attach(ctx, userAlice, backendPC, id, 0)
		if err != nil {
			t.Fatal(err)
		}
		next(t, ch)
		t.Cleanup(stop)
	}
	ss, _ = e.svc.Sessions(ctx, userAlice)
	needs := map[string]string{}
	for _, s := range ss {
		needs[s.ID] = s.Needs
	}
	if needs["s-ask"] != hub.NeedsApproval || needs["s-q"] != hub.NeedsQuestion || needs[sLive] != "" {
		t.Fatalf("needs = %v", needs)
	}
	if !slices.ContainsFunc(e.gw.Calls(), func(c gatewaytest.Call) bool {
		return c.Method == gateway.MethodSessionList && c.Params["picker"] == true
	}) {
		t.Fatal("session.list sent without picker")
	}
}

func TestSteerQueuedPrompt(t *testing.T) {
	t.Parallel()
	e := setup(t, fastSteer())
	running(t, e)
	res, err := e.svc.Prompt(hub.WithSteer(context.Background()), userAlice, backendPC, sLive, textSteer)
	if err != nil || !res.Steered || res.Held || res.ID == "" {
		t.Fatalf("prompt = %+v %v", res, err)
	}
	if got := e.gw.Steered(); len(got) != 1 {
		t.Fatalf("steered = %v", got)
	}
	if s, _ := e.gw.Session(sLive); len(s.Pending) != 1 || s.Pending[0].Status != gateway.AdmissionStarted {
		t.Fatalf("pending after steer = %+v", s.Pending)
	}
}

func TestSteerRetriesWhileTheTurnHasNoAgent(t *testing.T) {
	t.Parallel()
	e := setup(t, fastSteer())
	running(t, e)
	e.gw.SteerScript(gateway.ReasonSteerUnavailable, gateway.ReasonSteerUnavailable)
	res, err := e.svc.Prompt(hub.WithSteer(context.Background()), userAlice, backendPC, sLive, textSteer)
	if err != nil || !res.Steered {
		t.Fatalf("prompt = %+v %v", res, err)
	}
	if n := calls(e, gateway.MethodSteerQueued); n != 3 {
		t.Fatalf("steer calls = %d, want 3", n)
	}
}

func TestSteerRefusedStaysQueued(t *testing.T) {
	t.Parallel()
	e := setup(t, fastSteer())
	running(t, e)
	e.gw.SteerScript(gateway.ReasonUnsupportedControl)
	res, err := e.svc.Prompt(hub.WithSteer(context.Background()), userAlice, backendPC, sLive, textSteer)
	if err != nil || res.Steered || res.ID == "" {
		t.Fatalf("prompt = %+v %v", res, err)
	}
	if n := calls(e, gateway.MethodSteerQueued); n != 1 {
		t.Fatalf("steer calls = %d, want 1", n)
	}
	s, _ := e.gw.Session(sLive)
	if len(s.Pending) != 1 || s.Pending[0].Text != textSteer || s.Pending[0].Status != gateway.AdmissionQueued {
		t.Fatalf("pending = %+v", s.Pending)
	}
	e.gw.FinishTurn(sLive)
	waitFor(t, "the queued prompt to run", func() bool { return slices.Contains(userTexts(t, e), textSteer) })
}

func TestSteerWhenTheTurnEndsFirstRunsAsItsOwnTurn(t *testing.T) {
	t.Parallel()
	e := setup(t, fastSteer())
	running(t, e)
	e.gw.SteerScript(gatewaytest.SteerEnded)
	res, err := e.svc.Prompt(hub.WithSteer(context.Background()), userAlice, backendPC, sLive, textSteer)
	if err != nil || res.Steered || res.ID == "" {
		t.Fatalf("prompt = %+v %v", res, err)
	}
	waitFor(t, "the prompt to run as its own turn", func() bool {
		s, _ := e.gw.Session(sLive)
		return slices.Contains(userTexts(t, e), textSteer) && !s.Running
	})
	if len(e.gw.Steered()) != 0 {
		t.Fatalf("steered = %v", e.gw.Steered())
	}
}

func TestSteerSkipsIdleHeldAndFiles(t *testing.T) {
	t.Parallel()
	e := setup(t, fastSteer())
	ctx := hub.WithSteer(context.Background())
	e.gw.QueueWhileRunning = true
	res, err := e.svc.Prompt(ctx, userAlice, backendPC, sLive, "start")
	if err != nil || res.Steered {
		t.Fatalf("idle prompt = %+v %v", res, err)
	}
	waitFor(t, "the turn to end", func() bool { s, _ := e.gw.Session(sLive); return !s.Running })
	e.gw.BeginRestart()
	res, err = e.svc.Prompt(ctx, userAlice, backendPC, sLive, textSteer)
	if err != nil || !res.Held || res.Steered {
		t.Fatalf("held prompt = %+v %v", res, err)
	}
	if n := calls(e, gateway.MethodSteerQueued); n != 0 {
		t.Fatalf("steer calls = %d", n)
	}
}

func TestSlashCommandsRunThroughSlashExec(t *testing.T) {
	t.Parallel()
	e := setup(t, service.Options{})
	ctx := context.Background()
	res, err := e.svc.Prompt(ctx, userAlice, backendPC, sLive, "  "+cmdUsage)
	if err != nil || !res.Command || res.Output != "Tokens: 42" || res.ID != "" {
		t.Fatalf("usage = %+v %v", res, err)
	}
	res, err = e.svc.Prompt(ctx, userAlice, backendPC, sLive, "/plan fix the build")
	if err != nil || res.Command || res.ID == "" || res.Notice != "Planning first" {
		t.Fatalf("plan = %+v %v", res, err)
	}
	waitFor(t, "the plan prompt", func() bool { return slices.Contains(userTexts(t, e), "Make a plan for: fix the build") })
	if _, err := e.svc.Prompt(ctx, userAlice, backendPC, sLive, "/model big"); !errors.Is(err, hub.ErrUnknownCommand) {
		t.Fatalf("model = %v", err)
	}
	_, err = e.svc.Prompt(ctx, userAlice, backendPC, sLive, "/export")
	if !errors.Is(err, hub.ErrUnknownCommand) || !strings.Contains(err.Error(), "hermes profile export") {
		t.Fatalf("export = %v", err)
	}
	texts, _ := submitted(e)
	for _, x := range texts {
		if strings.HasPrefix(x, "/") {
			t.Fatalf("a slash command went to prompt.submit: %q", x)
		}
	}
	if got := e.gw.Commands(); !slices.Equal(got, []string{cmdUsage[1:], "plan fix the build", "model big", "export"}) {
		t.Fatalf("slash.exec got %q", got)
	}
	if _, err := e.svc.Prompt(ctx, userAlice, backendPC, sLive, "/diff"); !errors.Is(err, hub.ErrUnknownCommand) {
		t.Fatalf("diff = %v", err)
	}
	if _, err := e.svc.Prompt(ctx, userAlice, backendPC, sLive, "/etc/hosts is odd"); err != nil {
		t.Fatalf("a path as text = %v", err)
	}
	e.gw.BeginRestart()
	if _, err := e.svc.Prompt(ctx, userAlice, backendPC, sLive, cmdUsage); !errors.Is(err, hub.ErrBackendAway) {
		t.Fatalf("command while away = %v", err)
	}
}

func TestCompressAndCompactRunAsMutations(t *testing.T) {
	t.Parallel()
	e := setup(t, service.Options{})
	ctx := context.Background()
	res, err := e.svc.Prompt(ctx, userAlice, backendPC, sLive, "/compact keep the API")
	if err != nil || !res.Command || res.ID != "" || !strings.HasPrefix(res.Output, "Compressed: ") ||
		!strings.Contains(res.Output, "Approx request size") {
		t.Fatalf("compact = %+v %v", res, err)
	}
	res, err = e.svc.Prompt(ctx, userAlice, backendPC, sLive, " /COMPRESS --preview")
	if err != nil || !res.Command || !strings.HasPrefix(res.Output, "Would compress ") {
		t.Fatalf("preview = %+v %v", res, err)
	}
	if got := e.gw.Compressions(); !slices.Equal(got, []string{"keep the API", "--preview"}) {
		t.Fatalf("compress mutations = %q", got)
	}
	if got := e.gw.Commands(); len(got) != 0 {
		t.Fatalf("slash.exec got %q: /compress is a mutation", got)
	}
	texts, _ := submitted(e)
	for _, x := range texts {
		if strings.HasPrefix(x, "/") {
			t.Fatalf("a slash command went to prompt.submit: %q", x)
		}
	}
}

func TestCommandsCatalog(t *testing.T) {
	t.Parallel()
	e := setup(t, service.Options{})
	ctx := context.Background()
	cmds, err := e.svc.Commands(ctx, userAlice, backendPC, sLive)
	if err != nil {
		t.Fatal(err)
	}
	want := []hub.Command{
		{Name: "/title", Description: "Set the title", Args: "[name]"},
		{Name: cmdUsage, Description: "Show token usage"},
		{Name: "/compress", Description: "Compress context", Args: "[focus topic | --preview]", Aliases: []string{"/compact"}},
		{Name: "/plan", Description: "Plan first", Args: "<task>"},
		{Name: "/deploy", Description: "exec: make deploy"},
		{Name: "/my-skill", Description: "A skill", Skill: true},
		{Name: "/background", Description: "Run a prompt in the background, with this chat's settings", Args: "<prompt>",
			Aliases: []string{"/bg"}},
	}
	if !slices.EqualFunc(cmds, want, func(a, b hub.Command) bool {
		return a.Name == b.Name && a.Description == b.Description && a.Args == b.Args && a.Skill == b.Skill &&
			slices.Equal(a.Aliases, b.Aliases)
	}) {
		t.Fatalf("commands = %+v", cmds)
	}
	if _, err := e.svc.Commands(ctx, userAlice, backendPC, sLive); err != nil {
		t.Fatal(err)
	}
	if n := calls(e, gateway.MethodCommandsCatalog); n != 1 {
		t.Fatalf("catalog calls = %d, want 1 (cached)", n)
	}
	if _, err := e.svc.Commands(ctx, userBob, backendPC, sLive); !errors.Is(err, hub.ErrForbidden) {
		t.Fatalf("bob = %v", err)
	}
}

func userRows(t *testing.T, e *env, id string) []string {
	t.Helper()
	s, ok := e.gw.Session(id)
	if !ok {
		t.Fatalf("session %s gone", id)
	}
	out := make([]string, 0, len(s.Messages))
	for _, m := range s.Messages {
		out = append(out, fmt.Sprint(m[kRole], ":", m[kText]))
	}
	return out
}

func TestRewindEditAndRetry(t *testing.T) {
	t.Parallel()
	e := setup(t, service.Options{})
	ctx := context.Background()
	e.gw.AddSession(&gatewaytest.Session{ID: sessionNew, Live: true, StartedAt: 10, Messages: []map[string]any{
		{kRole: roleUser, kText: "first"}, {kRole: roleAsst, kText: "one"},
		{kRole: roleUser, kText: textOrig}, {kRole: roleAsst, kText: "two"}}})
	ch, stop, err := e.svc.Attach(ctx, userAlice, backendPC, sessionNew, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	_ = next(t, ch)
	res, err := e.svc.Rewind(ctx, userAlice, backendPC, sessionNew, hub.Rewind{Text: textEdit})
	if err != nil || res.Removed != 2 || res.Prompt.ID == "" {
		t.Fatalf("edit = %+v %v", res, err)
	}
	evs := until(t, ch, hub.KindSnapshot)
	if snap := string(evs[len(evs)-1].Data); strings.Contains(snap, `"two"`) {
		t.Fatalf("the snapshot after a rewind still has the rows Hermes took back: %s", snap)
	}
	until(t, ch, hub.KindMessageComplete)
	if got := userRows(t, e, sessionNew); !slices.Equal(got, []string{"user:first", "assistant:one", "user:" + textEdit, "assistant:echo: " + textEdit}) {
		t.Fatalf("rows after edit = %q", got)
	}
	res, err = e.svc.Rewind(ctx, userAlice, backendPC, sessionNew, hub.Rewind{Retry: true})
	if err != nil || res.Removed != 2 {
		t.Fatalf("retry = %+v %v", res, err)
	}
	until(t, ch, hub.KindMessageComplete)
	if got := userRows(t, e, sessionNew); got[2] != "user:"+textEdit {
		t.Fatalf("rows after retry = %q", got)
	}
	e.gw.RequireConfirm(modelBig)
	res, err = e.svc.Rewind(ctx, userAlice, backendPC, sessionNew, hub.Rewind{Retry: true, Model: modelBig})
	if err != nil || res.Model == nil || res.Model.Status != hub.ModelConfirm || res.Removed != 0 || res.Prompt.ID != "" {
		t.Fatalf("rewind with a model to confirm = %+v %v", res, err)
	}
	if s, _ := e.gw.Session(sessionNew); len(s.Messages) != 4 || s.Model == modelBig {
		t.Fatalf("a confirm changed the chat: %d rows, model %s", len(s.Messages), s.Model)
	}
	res, err = e.svc.Rewind(ctx, userAlice, backendPC, sessionNew, hub.Rewind{Text: "again", Model: modelBig, Confirm: "tok-" + modelBig})
	if err != nil || res.Model == nil || res.Model.Status != hub.ModelChanged || res.Removed != 2 {
		t.Fatalf("rewind with a model = %+v %v", res, err)
	}
	if s, _ := e.gw.Session(sessionNew); s.Model != modelBig {
		t.Fatalf("model = %s", s.Model)
	}
}

func TestRewindRefusals(t *testing.T) {
	t.Parallel()
	e := setup(t, service.Options{})
	ctx := context.Background()
	if _, err := e.svc.Rewind(ctx, userAlice, backendPC, "s-old", hub.Rewind{Retry: true}); !errors.Is(err, hub.ErrNoUserMessage) {
		t.Fatalf("no user message = %v", err)
	}
	if _, err := e.svc.Rewind(ctx, userAlice, backendPC, sLive, hub.Rewind{}); !errors.Is(err, hub.ErrBadValue) {
		t.Fatalf("empty = %v", err)
	}
	if _, err := e.svc.Rewind(ctx, userBob, backendPC, sLive, hub.Rewind{Retry: true}); !errors.Is(err, hub.ErrForbidden) {
		t.Fatalf("bob = %v", err)
	}
	e.gw.HoldTurns = true
	if _, err := e.svc.Prompt(ctx, userAlice, backendPC, sLive, "busy"); err != nil {
		t.Fatal(err)
	}
	ch, stop, err := e.svc.Attach(ctx, userAlice, backendPC, sLive, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	_ = next(t, ch)
	e.gw.StartElsewhere(sLive, "tui-2", "more", false, true)
	until(t, ch, hub.KindMessageStart)
	if _, err := e.svc.Rewind(ctx, userAlice, backendPC, sLive, hub.Rewind{Retry: true}); !errors.Is(err, hub.ErrBusy) {
		t.Fatalf("running = %v", err)
	}
}

func TestBranch(t *testing.T) {
	t.Parallel()
	e := setup(t, service.Options{})
	ctx := context.Background()
	e.gw.AddSession(&gatewaytest.Session{ID: sessionNew, Live: true, StartedAt: 10, Messages: []map[string]any{
		{kRole: roleUser, kText: "first"}, {kRole: roleAsst, kText: "one"},
		{kRole: roleUser, kText: textOrig}, {kRole: roleAsst, kText: "two"}}})
	child, err := e.svc.Branch(ctx, userAlice, backendPC, sessionNew, 2, titleCopy)
	if err != nil || child.ID == "" || child.Title != titleCopy || child.BackendID != backendPC {
		t.Fatalf("branch = %+v %v", child, err)
	}
	if e.gw.Parent(child.ID) != sessionNew {
		t.Fatalf("parent = %q", e.gw.Parent(child.ID))
	}
	if got := userRows(t, e, child.ID); !slices.Equal(got, []string{"user:first", "assistant:one"}) {
		t.Fatalf("branch rows = %q", got)
	}
	whole, err := e.svc.Branch(ctx, userAlice, backendPC, sessionNew, 0, "")
	if err != nil || len(userRows(t, e, whole.ID)) != 4 {
		t.Fatalf("whole branch = %+v %v", whole, err)
	}
	if _, err := e.svc.Branch(ctx, userAlice, backendPC, sessionNew, 99, ""); !errors.Is(err, hub.ErrBadValue) {
		t.Fatalf("unknown row = %v", err)
	}
	if _, err := e.svc.Branch(ctx, userAlice, backendPC, sessionNew, 0, strings.Repeat("x", 101)); !errors.Is(err, hub.ErrBadValue) {
		t.Fatalf("long title = %v", err)
	}
}

func TestTranscript(t *testing.T) {
	t.Parallel()
	e := setup(t, service.Options{})
	ctx := context.Background()
	msgs := make([]map[string]any, 1203)
	for i := range msgs {
		msgs[i] = map[string]any{kRole: roleUser, kText: fmt.Sprint("m", i)}
	}
	e.gw.AddSession(&gatewaytest.Session{ID: sessionNew, Title: "Long", StartedAt: 10, Messages: msgs})
	tr, err := e.svc.Transcript(ctx, userAlice, backendPC, sessionNew)
	if err != nil || tr.Title != "Long" || len(tr.Messages) != len(msgs) {
		t.Fatalf("transcript = %q %d %v", tr.Title, len(tr.Messages), err)
	}
	var last struct {
		RowID int64  `json:"row_id"`
		Text  string `json:"text"`
	}
	if err := json.Unmarshal(tr.Messages[len(msgs)-1], &last); err != nil || last.RowID != int64(len(msgs)) || last.Text != "m1202" {
		t.Fatalf("last row = %+v %v", last, err)
	}
	if calls(e, gateway.MethodSessionResume) != 0 {
		t.Fatal("the transcript subscribed to the session")
	}
	if _, err := e.svc.Transcript(ctx, userAlice, backendPC, "gone"); !errors.Is(err, hub.ErrUnknownSession) {
		t.Fatalf("unknown session = %v", err)
	}
}

func TestTranscriptWaitsOutARestart(t *testing.T) {
	t.Parallel()
	e := setup(t, service.Options{})
	ctx := context.Background()
	e.gw.AddSession(&gatewaytest.Session{ID: sessionNew, Title: "Long", StartedAt: 10,
		Messages: []map[string]any{{kRole: roleUser, kText: "hi"}}})
	if _, err := e.svc.Transcript(ctx, userAlice, backendPC, sessionNew); err != nil {
		t.Fatal(err)
	}
	e.gw.RestartConnections()
	e.gw.RefuseWhileDraining(gateway.MethodTranscript, 2)
	tr, err := e.svc.Transcript(ctx, userAlice, backendPC, sessionNew)
	if err != nil || len(tr.Messages) != 1 {
		t.Fatalf("transcript during a restart = %d %v", len(tr.Messages), err)
	}
}
