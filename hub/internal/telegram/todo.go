package telegram

import (
	"context"
	"fmt"
	"strings"
)

// todoEvent is Hermes's event with the whole to-do list after a todo tool
// ran (tui_gateway/tool_progress.py _on_tool_complete, todo.updated).
const todoEvent = "todo.updated"

// todoItem is one item of the to-do list (tools/todo_tool.py): status is
// pending, in_progress, completed or cancelled, and parent nests it under
// another item.
type todoItem struct {
	ID      string `json:"id"`
	Content string `json:"content"`
	Status  string `json:"status"`
	Parent  string `json:"parent"`
}

type todoPayload struct {
	Todos    []todoItem `json:"todos"`
	Revision int64      `json:"revision"`
}

// maxTodoItem bounds the text of one item.
const maxTodoItem = 200

// todoHTML is the to-do list as a compact message, items ticked off as
// they finish: ☐ to do, ☐ in bold for the one in progress, ☑ done and ☒
// cancelled. Telegram's checklists (sendChecklist) need a business
// connection, which this bot does not have.
func todoHTML(items []todoItem) string {
	if len(items) == 0 {
		return "<b>To-do</b> · cleared"
	}
	done := 0
	for _, it := range items {
		if it.Status == "completed" {
			done++
		}
	}
	depth := todoDepths(items)
	var b strings.Builder
	fmt.Fprintf(&b, "<b>To-do</b> · %d of %d done", done, len(items))
	for i, it := range items {
		line := "\n" + strings.Repeat("\u2003", depth[i])
		text := escapeHTML(truncate(strings.Join(strings.Fields(it.Content), " "), maxTodoItem))
		switch it.Status {
		case "completed":
			line += "☑ " + text
		case "cancelled":
			line += "☒ <s>" + text + "</s>"
		case "in_progress":
			line += "☐ <b>" + text + "</b>"
		default:
			line += "☐ " + text
		}
		if utf16Len(b.String())+utf16Len(line) > MaxMessageLen-40 {
			fmt.Fprintf(&b, "\n… %d more", len(items)-i)
			break
		}
		b.WriteString(line)
	}
	return b.String()
}

// todoDepths is how deep each item nests under its parents.
func todoDepths(items []todoItem) []int {
	parent := make(map[string]string, len(items))
	for _, it := range items {
		parent[it.ID] = it.Parent
	}
	out := make([]int, len(items))
	for i, it := range items {
		seen := map[string]bool{it.ID: true}
		for p := it.Parent; p != "" && !seen[p] && out[i] < 4; p = parent[p] {
			if _, ok := parent[p]; !ok {
				break
			}
			seen[p] = true
			out[i]++
		}
	}
	return out
}

// todoState is the message that shows the to-do list during a turn.
type todoState struct {
	msg liveMsg
	rev int64
}

// showTodos shows a new to-do list: in a new message for the first list
// of a turn, and after that by editing that message when the chat's
// budget allows.
func (r *renderer) showTodos(ctx context.Context, p todoPayload) {
	if p.Revision != 0 && p.Revision < r.todos.rev {
		return
	}
	r.todos.rev = p.Revision
	html := todoHTML(p.Todos)
	m := &r.todos.msg
	if m.id == 0 {
		if len(p.Todos) == 0 {
			return
		}
		r.update(ctx, m, html, nil)
		return
	}
	m.pending, m.dirty = html, true
	r.flushTodo(ctx, false)
}

// flushTodo edits the to-do message when an edit is pending and the
// chat's budget allows it, or at once when force is set.
func (r *renderer) flushTodo(ctx context.Context, force bool) {
	m := &r.todos.msg
	if !m.dirty || m.id == 0 {
		return
	}
	if !force && r.now().Before(m.retry) {
		return
	}
	if !force && r.budget != nil && m.pending != m.shown {
		if d := r.budget.take(r.to.chat); d > 0 {
			m.retry = r.now().Add(d)
			return
		}
	}
	r.edit(ctx, m, m.pending, nil)
}

// endTodo shows the last list of the turn, so the next turn's list starts
// a new message.
func (r *renderer) endTodo(ctx context.Context) {
	r.flushTodo(ctx, true)
	r.todos.msg = liveMsg{}
}
