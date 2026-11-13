package web

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"

	"github.com/USA-RedDragon/talaria/hub/internal/hub"
)

// pageSandbox are the sandbox flags of a page, in its CSP and on the
// page's iframe (app.js). Without allow-same-origin the page runs in an
// opaque origin of its own.
const pageSandbox = "allow-scripts allow-forms allow-modals allow-popups"

// pageScripts are the script hosts a page may load libraries from.
const pageScripts = "https://cdnjs.cloudflare.com/ajax/libs/ https://cdn.jsdelivr.net/npm/ https://unpkg.com/"

// pageCSP is the Content-Security-Policy of a page from origin, the hub's
// own origin, which alone may frame it.
func pageCSP(origin string) string {
	return "sandbox " + pageSandbox + "; default-src 'none'; " +
		"script-src 'unsafe-inline' 'unsafe-eval' " + pageScripts + "; " +
		"style-src 'unsafe-inline' https://fonts.googleapis.com/css https://fonts.googleapis.com/css2; " +
		"font-src https://fonts.gstatic.com/ data:; img-src data: blob:; media-src data: blob:; " +
		"connect-src 'none'; frame-src 'none'; worker-src 'none'; manifest-src 'none'; " +
		"form-action 'none'; base-uri 'none'; frame-ancestors " + origin
}

// pageBridge goes first in every page. It defines window.hermes: send(text)
// sends text to the chat as the user's message, draft(text) puts it in the
// chat's message box for the user to edit first, and copy(text) copies it,
// through the hub when the frame may not. send and draft answer a promise
// with {ok, error}. It talks to the hub only over the MessagePort that the
// hub's frame sends on the page's first load, so other windows cannot speak
// for the page. A page that is not in the hub's frame has no port, so send
// and draft copy the text instead.
const pageBridge = `<script>(function(){var port=null,queue=[],seq=0,waits={};` +
	`function post(m){if(port)port.postMessage(m);else if(queue.length<100)queue.push(m);}` +
	`addEventListener("message",function(e){if(port||e.source!==parent||!e.data||e.data.type!=="hermes-page:init"||!e.ports||!e.ports[0])return;` +
	`e.stopImmediatePropagation();port=e.ports[0];` +
	`port.onmessage=function(m){var d=m.data;if(d&&d.type==="result"&&waits[d.id]){var w=waits[d.id];delete waits[d.id];w({ok:!!d.ok,error:d.error?String(d.error):""});}};` +
	`for(var i=0;i<queue.length;i++)port.postMessage(queue[i]);queue=[];},true);` +
	`function copy(t){t=String(t);var ok=false;try{var a=document.createElement("textarea");a.value=t;a.setAttribute("readonly","");` +
	`a.style.position="fixed";a.style.opacity="0";(document.body||document.documentElement).appendChild(a);a.select();ok=document.execCommand("copy");a.remove();}catch(_){ok=false;}` +
	`post(ok?{type:"copied"}:{type:"copy",text:t});return ok;}` +
	`function ask(type,t){t=String(t);if(parent===window){var ok=copy(t);return Promise.resolve({ok:false,error:"This page is not open in the Talaria hub, so "+(ok?"the text was copied instead.":"it could not send. Copy the text yourself.")});}` +
	`var id=++seq;return new Promise(function(r){waits[id]=r;post({type:type,id:id,text:t});});}` +
	`window.hermes=Object.freeze({send:function(t){return ask("send",t);},draft:function(t){return ask("draft",t);},copy:copy});` +
	`addEventListener("click",function(e){var n=e.target&&e.target.closest&&e.target.closest("[data-hermes-send],[data-hermes-draft]");if(!n)return;e.preventDefault();` +
	`if(n.hasAttribute("data-hermes-send"))window.hermes.send(n.getAttribute("data-hermes-send"));else window.hermes.draft(n.getAttribute("data-hermes-draft"));});})();</script>`

// withBridge puts pageBridge into a page: after its doctype, so the page
// keeps its rendering mode, else at the start.
func withBridge(page []byte) []byte {
	page = bytes.TrimPrefix(page, []byte("\xef\xbb\xbf"))
	at := 0
	rest := page
	for {
		t := bytes.TrimLeft(rest, " \t\r\n\f")
		if !bytes.HasPrefix(t, []byte("<!--")) {
			rest = t
			break
		}
		end := bytes.Index(t[4:], []byte("-->"))
		if end < 0 {
			rest = t
			break
		}
		rest = t[4+end+3:]
	}
	if len(rest) >= 9 && bytes.EqualFold(rest[:9], []byte("<!doctype")) {
		if end := bytes.IndexByte(rest, '>'); end >= 0 {
			at = len(page) - len(rest) + end + 1
		}
	}
	out := make([]byte, 0, len(page)+len(pageBridge))
	out = append(out, page[:at]...)
	out = append(out, pageBridge...)
	return append(out, page[at:]...)
}

// pageHash is the hub route that opens a page in its frame.
func pageHash(backendID, sessionID, ref string) string {
	return "/#/page/" + url.PathEscape(backendID) + "/" + url.PathEscape(sessionID) + "/" + url.PathEscape(ref)
}

// handlePage serves a page for the hub's sandboxed frame: a page block of a
// reply (?id=<PageID>) or an HTML file that a reply names with a MEDIA
// directive (?path=). The page runs in an opaque origin under pageCSP, so
// it cannot read the hub, call its API or share storage with other pages.
// A request that is not for a frame goes to the hub page that frames it.
func (s *Server) handlePage(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Del("X-Frame-Options")
	h.Set("Content-Security-Policy", pageCSP(s.origin))
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("X-DNS-Prefetch-Control", "off")
	h.Set("Cache-Control", "no-store")
	h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=(), display-capture=(), usb=(), payment=()")
	backendID, sessionID := r.PathValue("backend"), r.PathValue("session")
	id, p := r.URL.Query().Get("id"), r.URL.Query().Get("path")
	ref := id
	if id == "" {
		ref = p
	}
	if dest := r.Header.Get("Sec-Fetch-Dest"); dest != "" && dest != "iframe" {
		http.Redirect(w, r, pageHash(backendID, sessionID, ref), http.StatusSeeOther) //nolint:gosec // a fragment on the hub's own root; every part is escaped
		return
	}
	page, err := s.readPage(r, backendID, sessionID, id, p)
	if err != nil {
		s.pageError(w, err)
		return
	}
	body := withBridge(page)
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Content-Length", strconv.Itoa(len(body)))
	_, _ = w.Write(body) //nolint:gosec // agent HTML on purpose; pageCSP sandboxes it into an opaque origin
}

// handlePageInfo answers the title of an HTML file that a reply names, for
// its card. Cards of page blocks read the title from the reply.
func (s *Server) handlePageInfo(w http.ResponseWriter, r *http.Request) {
	page, err := s.readPage(r, r.PathValue("backend"), r.PathValue("session"), "", r.URL.Query().Get("path"))
	if err != nil {
		s.pageError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"title": hub.PageTitle(string(page))})
}

func (s *Server) readPage(r *http.Request, backendID, sessionID, id, p string) ([]byte, error) {
	user := userFrom(r)
	switch {
	case id != "" && p == "":
		pages, ok := s.svc.(hub.Pages)
		if !ok || !hub.PageIDOK(id) {
			return nil, hub.ErrNoPage
		}
		page, err := pages.Page(r.Context(), user, backendID, sessionID, id)
		return []byte(page), err
	case id == "" && hub.PagePath(p):
		m, ok := s.svc.(hub.Media)
		if !ok {
			return nil, hub.ErrNoPage
		}
		f, err := m.Media(r.Context(), user, backendID, sessionID, p)
		if errors.Is(err, hub.ErrNoMedia) {
			return nil, hub.ErrNoPage
		}
		if err != nil {
			return nil, err
		}
		defer f.Body.Close()
		if f.Size > hub.MaxPageBytes {
			return nil, hub.ErrMediaTooLarge
		}
		data, err := io.ReadAll(io.LimitReader(f.Body, hub.MaxPageBytes+1))
		if err != nil {
			return nil, err
		}
		if len(data) > hub.MaxPageBytes {
			return nil, hub.ErrMediaTooLarge
		}
		return data, nil
	}
	return nil, hub.ErrNoPage
}

func (s *Server) pageError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, hub.ErrNoPage):
		errorJSON(w, http.StatusNotFound, hub.ErrNoPage.Error())
	case errors.Is(err, hub.ErrMediaTooLarge):
		errorJSON(w, http.StatusRequestEntityTooLarge, "the page is too large")
	case errors.Is(err, hub.ErrMediaOffline):
		errorJSON(w, http.StatusServiceUnavailable, hub.ErrMediaOffline.Error())
	default:
		s.fail(w, err)
	}
}
