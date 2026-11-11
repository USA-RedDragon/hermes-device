package telegram_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/USA-RedDragon/hermes-hub/internal/hub"
	"github.com/USA-RedDragon/hermes-hub/internal/notify"
	"github.com/USA-RedDragon/hermes-hub/internal/telegram"
)

const (
	accountID   = "acc1"
	goodCode    = "c0de-g00d"
	signInURL   = "https://claude.example/oauth?x=1"
	mDelete     = "deleteMessage"
	accountName = "Claude (account1)"
)

// fakeClaude is a hub.Claude with one account whose admin is Alice.
type fakeClaude struct {
	mu       sync.Mutex
	si       hub.ClaudeSignIn
	startErr error
	codes    []string
	ttl      time.Duration
}

func (f *fakeClaude) admin(user string) error {
	if user != userAlice {
		return hub.ErrForbidden
	}
	return nil
}

func (f *fakeClaude) ClaudeAccounts(context.Context, string, string, bool) (hub.ClaudeAccounts, error) {
	return hub.ClaudeAccounts{}, nil
}

func (f *fakeClaude) ClaudeUsage(context.Context, string, string, string, bool) (hub.ClaudeUsage, error) {
	return hub.ClaudeUsage{}, nil
}

func (f *fakeClaude) StartSignIn(_ context.Context, user, _, account string) (hub.ClaudeSignIn, error) {
	if err := f.admin(user); err != nil {
		return hub.ClaudeSignIn{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.startErr != nil {
		return hub.ClaudeSignIn{}, f.startErr
	}
	ttl := f.ttl
	if ttl == 0 {
		ttl = time.Hour
	}
	f.si = hub.ClaudeSignIn{ID: "si1", Account: account, Label: accountName, EmailHint: "old@example.com", State: hub.SignInWaiting,
		URL: signInURL, ExpiresAt: time.Now().Add(ttl)}
	return f.si, nil
}

func (f *fakeClaude) SignInState(_ context.Context, user, _, _ string) (hub.ClaudeSignIn, error) {
	if err := f.admin(user); err != nil {
		return hub.ClaudeSignIn{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.si.ID == "" {
		return hub.ClaudeSignIn{}, hub.ErrNoSignIn
	}
	return f.si, nil
}

func (f *fakeClaude) SendSignInCode(_ context.Context, user, _, _, signInID, code string) (hub.ClaudeSignIn, error) {
	if err := f.admin(user); err != nil {
		return hub.ClaudeSignIn{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.codes = append(f.codes, code)
	if signInID != f.si.ID {
		return hub.ClaudeSignIn{}, hub.ErrWrongSignIn
	}
	if code != goodCode {
		return hub.ClaudeSignIn{}, hub.ErrBadCode
	}
	f.si.State, f.si.Email = hub.SignInSucceeded, "new@example.com"
	return f.si, nil
}

func (f *fakeClaude) CancelSignIn(context.Context, string, string, string) (hub.ClaudeSignIn, error) {
	return hub.ClaudeSignIn{}, nil
}

func (f *fakeClaude) ClaudePool(context.Context, string, string, string, string) (hub.ClaudePool, error) {
	return hub.ClaudePool{}, nil
}

func (f *fakeClaude) codesCopy() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.codes...)
}

func claudeNotice() hub.NoticeRecord {
	return hub.NoticeRecord{ID: "nC", User: userAlice, Kind: hub.NoticeClaudeSignIn, BackendID: beCluster, Account: accountID,
		CreatedAt: time.Now()}
}

func withClaude(c hub.Claude) func(*telegram.Options) {
	return func(o *telegram.Options) { o.Claude = c }
}

func TestClaudeSignInFromTelegram(t *testing.T) {
	t.Parallel()
	fc := &fakeClaude{}
	h := startBotNotices(t, nil, newFakeState(), nil, &fakeNotices{}, withClaude(fc))
	id, err := h.bot.SendNotice(context.Background(), aliceTG, notify.Message{Record: claudeNotice(), Backend: beName,
		Detail: "needs a sign-in. Open the hub to sign in."})
	if err != nil {
		t.Fatal(err)
	}
	link := h.tg.wait(mSend, accountName+", last signed in as old@example.com")
	markup, _ := link.Params["reply_markup"].(map[string]any)
	if link.ResultID != id || markup["force_reply"] != true || !strings.Contains(link.text(), `href="`+strings.ReplaceAll(signInURL, "&", "&amp;")+`"`) {
		t.Fatalf("link = %+v", link.Params)
	}
	h.tg.replyTo(dm(aliceTG), bobTG, id, goodCode)
	h.tg.wait(mSend, "Only the person this sign-in was sent to")
	h.tg.replyTo(dm(aliceTG), aliceTG, id, "/status")
	h.tg.wait(mSend, "No chat is attached.")
	h.tg.replyTo(dm(aliceTG), aliceTG, id, "wrong")
	h.tg.wait(mSend, "That code was not accepted")
	h.tg.replyTo(dm(aliceTG), aliceTG, id, goodCode)
	edit := h.tg.wait(mEdit, "<b>Signed in as new@example.com</b>")
	if edit.messageID() != id || !strings.Contains(edit.text(), accountName) {
		t.Fatalf("edit = %+v", edit)
	}
	if got := fc.codesCopy(); len(got) != 2 || got[0] != "wrong" || got[1] != goodCode {
		t.Fatalf("codes = %v", got)
	}
	if n := len(h.tg.find(mDelete, "")); n != 2 {
		t.Fatalf("deleted %d code messages, want 2", n)
	}
	for _, c := range h.tg.snapshot() {
		if strings.Contains(c.text(), goodCode) {
			t.Fatalf("the code went out in %s: %q", c.Method, c.text())
		}
	}
	if n := len(h.hub.promptsCopy()); n != 0 {
		t.Fatalf("a code went to Hermes as a prompt: %d", n)
	}
}

func TestClaudeSignInExpires(t *testing.T) {
	t.Parallel()
	fc := &fakeClaude{ttl: 50 * time.Millisecond}
	h := startBotNotices(t, nil, newFakeState(), nil, &fakeNotices{}, withClaude(fc))
	id, err := h.bot.SendNotice(context.Background(), aliceTG, notify.Message{Record: claudeNotice(), Backend: beName})
	if err != nil {
		t.Fatal(err)
	}
	if edit := h.tg.wait(mEdit, "<b>Expired</b>"); edit.messageID() != id {
		t.Fatalf("edit = %+v", edit)
	}
}

func TestClaudeSignInFallsBackToNotice(t *testing.T) {
	t.Parallel()
	fc := &fakeClaude{startErr: errors.New("plugin down")}
	h := startBotNotices(t, nil, newFakeState(), nil, &fakeNotices{}, withClaude(fc))
	rec := claudeNotice()
	if _, err := h.bot.SendNotice(context.Background(), aliceTG, notify.Message{Record: rec, Detail: "Open the hub to sign in."}); err != nil {
		t.Fatal(err)
	}
	if got := h.tg.wait(mSend, "Open the hub to sign in."); strings.Contains(got.text(), signInURL) {
		t.Fatalf("notice = %q", got.text())
	}
	rec.User = userBob
	fc.mu.Lock()
	fc.startErr = nil
	fc.mu.Unlock()
	if _, err := h.bot.SendNotice(context.Background(), bobTG, notify.Message{Record: rec, Detail: "for bob"}); err != nil {
		t.Fatal(err)
	}
	if got := h.tg.wait(mSend, "for bob"); strings.Contains(got.text(), "claude.example") {
		t.Fatalf("a non-admin got the sign-in link: %q", got.text())
	}
}

func TestClaudeCodeToStoredNotice(t *testing.T) {
	t.Parallel()
	fc := &fakeClaude{}
	if _, err := fc.StartSignIn(context.Background(), userAlice, beCluster, accountID); err != nil {
		t.Fatal(err)
	}
	rec := claudeNotice()
	notices := &fakeNotices{recs: map[string]hub.NoticeRecord{rec.ID: rec},
		msgs: map[string][]hub.NoticeMessage{rec.ID: {{ChatID: aliceTG, MessageID: 777}}}}
	h := startBotNotices(t, nil, newFakeState(), nil, notices, withClaude(fc))
	h.tg.replyTo(dm(aliceTG), aliceTG, 777, goodCode)
	if edit := h.tg.wait(mEdit, "Signed in as new@example.com"); edit.messageID() != 777 {
		t.Fatalf("edit = %+v", edit)
	}
}
