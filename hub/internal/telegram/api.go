package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// ErrRateLimited is returned when Telegram keeps answering 429 after every retry.
var ErrRateLimited = errors.New("telegram rate limit retries exhausted")

// APIError is an error answer from the Bot API.
type APIError struct {
	Method      string
	Code        int
	Description string
	RetryAfter  time.Duration
}

func (e *APIError) Error() string {
	return fmt.Sprintf("telegram %s: %d %s", e.Method, e.Code, e.Description)
}

// NotModified reports whether the error is Telegram refusing an edit that
// changes nothing.
func (e *APIError) NotModified() bool {
	return e.Code == http.StatusBadRequest && strings.Contains(e.Description, "message is not modified")
}

// TopicGone reports whether Telegram refused a message because its forum
// topic was deleted or closed.
func (e *APIError) TopicGone() bool {
	d := strings.ToLower(e.Description)
	return e.Code == http.StatusBadRequest &&
		(strings.Contains(d, "message thread not found") || strings.Contains(d, "topic_deleted") || strings.Contains(d, "topic_closed"))
}

// BadEntities reports whether Telegram could not parse the HTML markup.
func (e *APIError) BadEntities() bool {
	return e.Code == http.StatusBadRequest && strings.Contains(e.Description, "can't parse entities")
}

// User is a Telegram user.
type User struct {
	ID        int64  `json:"id"`
	IsBot     bool   `json:"is_bot"`
	FirstName string `json:"first_name"`
	Username  string `json:"username,omitempty"`
}

// Chat is a Telegram chat.
type Chat struct {
	ID   int64  `json:"id"`
	Type string `json:"type"`
	// IsForum is true for a supergroup with Topics turned on.
	IsForum bool `json:"is_forum,omitempty"`
}

// ForumTopic is a topic of a forum supergroup.
type ForumTopic struct {
	MessageThreadID int64  `json:"message_thread_id,omitempty"`
	Name            string `json:"name"`
}

// Message is a Telegram message.
type Message struct {
	MessageID int64 `json:"message_id"`
	From      *User `json:"from,omitempty"`
	Chat      Chat  `json:"chat"`
	// MessageThreadID is the forum topic of a message in a topic.
	MessageThreadID int64 `json:"message_thread_id,omitempty"`
	IsTopicMessage  bool  `json:"is_topic_message,omitempty"`
	// ReplyToMessage is the message this one replies to. In a forum topic
	// it is the topic's first message when the person did not reply to
	// anything.
	ReplyToMessage    *Message    `json:"reply_to_message,omitempty"`
	ForumTopicCreated *ForumTopic `json:"forum_topic_created,omitempty"`
	Text              string      `json:"text,omitempty"`
	Caption           string      `json:"caption,omitempty"`
	MediaGroupID      string      `json:"media_group_id,omitempty"`
	Photo             []PhotoSize `json:"photo,omitempty"`
	Document          *File       `json:"document,omitempty"`
	Video             *File       `json:"video,omitempty"`
	Audio             *File       `json:"audio,omitempty"`
	Voice             *File       `json:"voice,omitempty"`
}

// PhotoSize is one size of a photo.
type PhotoSize struct {
	FileID   string `json:"file_id"`
	FileSize int64  `json:"file_size,omitempty"`
}

// File is a document, video, audio file or voice note: the fields they
// share.
type File struct {
	FileID   string `json:"file_id"`
	FileName string `json:"file_name,omitempty"`
	MIMEType string `json:"mime_type,omitempty"`
	FileSize int64  `json:"file_size,omitempty"`
}

// MaxDownload is the largest file the Bot API lets a bot download
// (https://core.telegram.org/bots/api#getfile).
const MaxDownload = 20 << 20

// CallbackQuery is a press on an inline keyboard button.
type CallbackQuery struct {
	ID      string   `json:"id"`
	From    User     `json:"from"`
	Message *Message `json:"message,omitempty"`
	Data    string   `json:"data,omitempty"`
}

// Update is one item from getUpdates.
type Update struct {
	UpdateID      int64          `json:"update_id"`
	Message       *Message       `json:"message,omitempty"`
	CallbackQuery *CallbackQuery `json:"callback_query,omitempty"`
}

// InlineButton is one inline keyboard button: a callback, or a link.
type InlineButton struct {
	Text         string `json:"text"`
	CallbackData string `json:"callback_data,omitempty"`
	URL          string `json:"url,omitempty"`
}

// ForceReply makes the person's app open a reply to the message.
type ForceReply struct {
	ForceReply  bool   `json:"force_reply"`
	Placeholder string `json:"input_field_placeholder,omitempty"`
}

// InlineKeyboard is an inline keyboard markup.
type InlineKeyboard struct {
	InlineKeyboard [][]InlineButton `json:"inline_keyboard"`
}

type linkPreview struct {
	IsDisabled bool `json:"is_disabled"`
}

type sendMessageParams struct {
	ChatID          int64        `json:"chat_id"`
	MessageThreadID int64        `json:"message_thread_id,omitempty"`
	Text            string       `json:"text"`
	ParseMode       string       `json:"parse_mode,omitempty"`
	ReplyMarkup     any          `json:"reply_markup,omitempty"`
	LinkPreview     *linkPreview `json:"link_preview_options,omitempty"`
}

// markup is kb as a reply_markup value, nil when kb is nil.
func markup(kb *InlineKeyboard) any {
	if kb == nil {
		return nil
	}
	return kb
}

type editMessageParams struct {
	ChatID      int64        `json:"chat_id"`
	MessageID   int64        `json:"message_id"`
	Text        string       `json:"text"`
	ParseMode   string       `json:"parse_mode,omitempty"`
	ReplyMarkup any          `json:"reply_markup,omitempty"`
	LinkPreview *linkPreview `json:"link_preview_options,omitempty"`
}

type getUpdatesParams struct {
	Offset         int64    `json:"offset,omitempty"`
	Timeout        int      `json:"timeout"`
	AllowedUpdates []string `json:"allowed_updates"`
}

type answerCallbackParams struct {
	CallbackQueryID string `json:"callback_query_id"`
	Text            string `json:"text,omitempty"`
}

type apiResponse struct {
	OK          bool            `json:"ok"`
	Result      json.RawMessage `json:"result"`
	ErrorCode   int             `json:"error_code"`
	Description string          `json:"description"`
	Parameters  *struct {
		RetryAfter int `json:"retry_after"`
	} `json:"parameters"`
}

// Client is a small Bot API client. It retries 429 answers after the
// retry_after Telegram sends.
type Client struct {
	base       string
	fileBase   string
	http       *http.Client
	maxRetries int
	logger     *slog.Logger
}

// NewClient returns a Client for token against apiURL.
func NewClient(apiURL, token string, httpClient *http.Client, maxRetries int, logger *slog.Logger) *Client {
	if httpClient == nil {
		httpClient = &http.Client{}
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Client{
		base:       strings.TrimRight(apiURL, "/") + "/bot" + token + "/",
		fileBase:   strings.TrimRight(apiURL, "/") + "/file/bot" + token + "/",
		http:       httpClient,
		maxRetries: maxRetries,
		logger:     logger,
	}
}

func (c *Client) call(ctx context.Context, method string, params, result any) error {
	body, err := json.Marshal(params)
	if err != nil {
		return fmt.Errorf("failed to encode %s: %w", method, err)
	}
	return c.callBody(ctx, method, body, "application/json", result)
}

// callBody sends a request body as it is, retrying while Telegram rate
// limits it.
func (c *Client) callBody(ctx context.Context, method string, body []byte, contentType string, result any) error {
	for attempt := 0; ; attempt++ {
		err := c.do(ctx, method, body, contentType, result)
		var apiErr *APIError
		if !errors.As(err, &apiErr) || apiErr.Code != http.StatusTooManyRequests {
			return err
		}
		if attempt >= c.maxRetries {
			return fmt.Errorf("%w: %w", ErrRateLimited, err)
		}
		wait := max(apiErr.RetryAfter, time.Second)
		c.logger.Warn("Telegram rate limit, retrying", "method", method, "retry_after", wait, "attempt", attempt+1)
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return fmt.Errorf("telegram %s: %w", method, ctx.Err())
		case <-t.C:
		}
	}
}

func (c *Client) do(ctx context.Context, method string, body []byte, contentType string, result any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+method, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("failed to build %s request: %w", method, err)
	}
	req.Header.Set("Content-Type", contentType)
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("telegram %s: %w", method, redactToken(err))
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return fmt.Errorf("failed to read %s response: %w", method, err)
	}
	var r apiResponse
	if err := json.Unmarshal(raw, &r); err != nil {
		return fmt.Errorf("failed to decode %s response (HTTP %d): %w", method, resp.StatusCode, err)
	}
	if !r.OK {
		apiErr := &APIError{Method: method, Code: r.ErrorCode, Description: r.Description}
		if apiErr.Code == 0 {
			apiErr.Code = resp.StatusCode
		}
		if r.Parameters != nil {
			apiErr.RetryAfter = time.Duration(r.Parameters.RetryAfter) * time.Second
		}
		return apiErr
	}
	if result == nil {
		return nil
	}
	if err := json.Unmarshal(r.Result, result); err != nil {
		return fmt.Errorf("failed to decode %s result: %w", method, err)
	}
	return nil
}

// redactToken drops the request URL, which holds the bot token, from
// transport errors.
func redactToken(err error) error {
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return urlErr.Err
	}
	return err
}

// GetUpdates long-polls for updates after offset.
func (c *Client) GetUpdates(ctx context.Context, offset int64, timeout time.Duration) ([]Update, error) {
	var out []Update
	err := c.call(ctx, "getUpdates", getUpdatesParams{
		Offset:         offset,
		Timeout:        int(timeout / time.Second),
		AllowedUpdates: []string{"message", "callback_query"},
	}, &out)
	return out, err
}

// SendHTML sends an HTML message to a chat, or to a topic of a forum
// when thread is not 0, and returns its ID.
func (c *Client) SendHTML(ctx context.Context, chatID, thread int64, html string, kb *InlineKeyboard) (int64, error) {
	var m Message
	err := c.call(ctx, "sendMessage", sendMessageParams{
		ChatID: chatID, MessageThreadID: thread, Text: html, ParseMode: parseHTML, ReplyMarkup: markup(kb),
		LinkPreview: &linkPreview{IsDisabled: true},
	}, &m)
	return m.MessageID, err
}

// SendForceReply sends an HTML message that opens a reply to itself in
// the person's app, and returns its ID.
func (c *Client) SendForceReply(ctx context.Context, chatID, thread int64, html, placeholder string) (int64, error) {
	var m Message
	err := c.call(ctx, "sendMessage", sendMessageParams{
		ChatID: chatID, MessageThreadID: thread, Text: html, ParseMode: parseHTML,
		ReplyMarkup: ForceReply{ForceReply: true, Placeholder: truncate(placeholder, 64)},
		LinkPreview: &linkPreview{IsDisabled: true},
	}, &m)
	return m.MessageID, err
}

// SendText sends a plain text message and returns its ID.
func (c *Client) SendText(ctx context.Context, chatID, thread int64, text string) (int64, error) {
	var m Message
	err := c.call(ctx, "sendMessage", sendMessageParams{
		ChatID: chatID, MessageThreadID: thread, Text: text, LinkPreview: &linkPreview{IsDisabled: true},
	}, &m)
	return m.MessageID, err
}

type topicParams struct {
	ChatID          int64  `json:"chat_id"`
	MessageThreadID int64  `json:"message_thread_id,omitempty"`
	Name            string `json:"name"`
}

// parseHTML is the parse_mode of messages in HTML.
const parseHTML = "HTML"

// maxTopicName is the longest forum topic name Telegram takes.
const maxTopicName = 128

// CreateForumTopic creates a topic in a forum supergroup and returns its
// thread ID.
func (c *Client) CreateForumTopic(ctx context.Context, chatID int64, name string) (int64, error) {
	var t ForumTopic
	err := c.call(ctx, "createForumTopic", topicParams{ChatID: chatID, Name: truncate(name, maxTopicName)}, &t)
	return t.MessageThreadID, err
}

// EditForumTopic renames a topic. A rename that changes nothing is not an
// error.
func (c *Client) EditForumTopic(ctx context.Context, chatID, thread int64, name string) error {
	err := c.call(ctx, "editForumTopic", topicParams{ChatID: chatID, MessageThreadID: thread, Name: truncate(name, maxTopicName)}, nil)
	var apiErr *APIError
	if errors.As(err, &apiErr) && strings.Contains(apiErr.Description, "TOPIC_NOT_MODIFIED") {
		return nil
	}
	return err
}

// ChatMember is what getChatMember says of a member of a chat.
type ChatMember struct {
	Status          string `json:"status"`
	CanManageTopics bool   `json:"can_manage_topics"`
}

type chatMemberParams struct {
	ChatID int64 `json:"chat_id"`
	UserID int64 `json:"user_id"`
}

// GetChatMember reads a member of a chat.
func (c *Client) GetChatMember(ctx context.Context, chatID, userID int64) (ChatMember, error) {
	var m ChatMember
	err := c.call(ctx, "getChatMember", chatMemberParams{ChatID: chatID, UserID: userID}, &m)
	return m, err
}

// EditHTML replaces the text of a message. An edit that changes nothing is
// not an error.
func (c *Client) EditHTML(ctx context.Context, chatID, messageID int64, html string, kb *InlineKeyboard) error {
	err := c.call(ctx, "editMessageText", editMessageParams{
		ChatID: chatID, MessageID: messageID, Text: html, ParseMode: parseHTML, ReplyMarkup: markup(kb),
		LinkPreview: &linkPreview{IsDisabled: true},
	}, nil)
	var apiErr *APIError
	if errors.As(err, &apiErr) && apiErr.NotModified() {
		return nil
	}
	return err
}

type editMarkupParams struct {
	ChatID      int64           `json:"chat_id"`
	MessageID   int64           `json:"message_id"`
	ReplyMarkup *InlineKeyboard `json:"reply_markup,omitempty"`
}

// EditMarkup replaces the buttons of a message; nil removes them. An edit
// that changes nothing is not an error.
func (c *Client) EditMarkup(ctx context.Context, chatID, messageID int64, kb *InlineKeyboard) error {
	err := c.call(ctx, "editMessageReplyMarkup", editMarkupParams{ChatID: chatID, MessageID: messageID, ReplyMarkup: kb}, nil)
	var apiErr *APIError
	if errors.As(err, &apiErr) && apiErr.NotModified() {
		return nil
	}
	return err
}

// AnswerCallback acknowledges a button press, with an optional toast.
func (c *Client) AnswerCallback(ctx context.Context, id, text string) error {
	return c.call(ctx, "answerCallbackQuery", answerCallbackParams{CallbackQueryID: id, Text: text}, nil)
}

type getFileParams struct {
	FileID string `json:"file_id"`
}

// ErrTooLarge is a file over the download limit.
var ErrTooLarge = errors.New("file too large to download")

// Download fetches a file the user sent, up to limit bytes.
func (c *Client) Download(ctx context.Context, fileID string, limit int64) ([]byte, error) {
	var f struct {
		FilePath string `json:"file_path"`
		FileSize int64  `json:"file_size"`
	}
	if err := c.call(ctx, "getFile", getFileParams{FileID: fileID}, &f); err != nil {
		return nil, err
	}
	if f.FileSize > limit {
		return nil, ErrTooLarge
	}
	if f.FilePath == "" || strings.HasPrefix(f.FilePath, "/") || strings.Contains(f.FilePath, "..") {
		return nil, fmt.Errorf("telegram getFile: unusable file path")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.fileBase+f.FilePath, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to build file request: %w", redactToken(err))
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("telegram file download: %w", redactToken(err))
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("telegram file download: HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, fmt.Errorf("telegram file download: %w", redactToken(err))
	}
	if int64(len(data)) > limit {
		return nil, ErrTooLarge
	}
	return data, nil
}

// Upload limits of the Bot API (https://core.telegram.org/bots/api#sending-files).
const (
	MaxPhotoUpload    = 10 << 20
	MaxDocumentUpload = 50 << 20
)

// SendPhoto sends an image that the chat shows inline.
func (c *Client) SendPhoto(ctx context.Context, chatID, thread int64, name string, data []byte) error {
	return c.upload(ctx, "sendPhoto", "photo", chatID, thread, name, data)
}

// SendDocument sends a file.
func (c *Client) SendDocument(ctx context.Context, chatID, thread int64, name string, data []byte) error {
	return c.upload(ctx, "sendDocument", "document", chatID, thread, name, data)
}

func (c *Client) upload(ctx context.Context, method, field string, chatID, thread int64, name string, data []byte) error {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	if err := w.WriteField("chat_id", strconv.FormatInt(chatID, 10)); err != nil {
		return err
	}
	if thread != 0 {
		if err := w.WriteField("message_thread_id", strconv.FormatInt(thread, 10)); err != nil {
			return err
		}
	}
	fw, err := w.CreateFormFile(field, name)
	if err != nil {
		return err
	}
	if _, err := fw.Write(data); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	return c.callBody(ctx, method, buf.Bytes(), w.FormDataContentType(), nil)
}
