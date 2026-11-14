package web_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/USA-RedDragon/talaria/hub/internal/gateway/gatewaytest"
)

// activationPage is a page that sends at load, on clicks, in a loop and on
// command from the test. It reports every answer to the hub window, where
// run.js reads it, and the position of every button.
const activationPage = `<!doctype html><title>Act</title><style>body{margin:0}button{display:block;width:120px;height:20px;margin:0}</style>
<button id="one">one</button><button id="loop">loop</button><button id="key">key</button>
<button id="ds" data-hermes-send="ds">ds</button><button id="hd">hd</button><button id="dd" data-hermes-draft="dd">dd</button>
<script>
var rep=function(t,r){parent.postMessage({t:t,r:r},"*");};
var go=function(t,p){p.then(function(r){rep(t,r);});};
var timer=0;
document.getElementById("one").onclick=function(){go("one",hermes.send("one"));};
document.getElementById("loop").onclick=function(){for(var i=0;i<5;i++)go("loop"+i,hermes.send("loop"+i));};
// A key press, not a click: headless Firefox does not run a button's
// default action for a synthesized Enter, so the page reads the key itself.
document.getElementById("key").onkeydown=function(e){if(e.key==="Enter")go("key",hermes.send("key"));};
document.getElementById("hd").onclick=function(){go("hd",hermes.draft("hd"));};
addEventListener("message",function(e){var d=e.data||{};
if(d.cmd==="send")go("auto",hermes.send("auto"));
else if(d.cmd==="draft")go("auto-draft",hermes.draft("auto-draft"));
else if(d.cmd==="click")document.getElementById(d.id).click();
else if(d.cmd==="focus"){document.getElementById(d.id).focus();rep("focus",[document.activeElement.id,document.hasFocus()]);}
else if(d.cmd==="loopsend")timer=setInterval(function(){hermes.send("burst");},50);
else if(d.cmd==="stop")clearInterval(timer);
else if(d.cmd==="copy"){hermes.copy("copied text");rep("copy",true);}
else if(d.cmd==="rects"){var o={};["one","loop","key","ds","hd","dd"].forEach(function(id){var r=document.getElementById(id).getBoundingClientRect();o[id]=[r.left+r.width/2,r.top+r.height/2];});parent.postMessage({rects:o},"*");}
});
go("onload",hermes.send("at load"));
</script>`

// A page's hermes.send, data-hermes-send and hermes.draft go through only
// right after the user's own click or key press in the page, once per
// click; a send from the page's own script, at load, in a loop or from a
// click event it made itself is refused. testdata/pageact/run.js drives the
// real hub page with real mouse and key input, in headless Chrome and in
// headless Firefox. Each skips where node or its browser is missing.
//
// The same run.js drives both; run.js picks Firefox when the path says so.
func TestPageSendNeedsUserActivationChrome(t *testing.T) {
	t.Parallel()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	bin := chromePath(t)
	e := setup(t)
	reply := "Here is the page.\n\n```page\n" + activationPage + "\n```\n"
	e.gw.AddSession(&gatewaytest.Session{ID: "s9", Title: "Pages to click", Live: true, StartedAt: 900, Messages: []map[string]any{
		{kRole: roleUser, kText: "make the click page"}, {kRole: roleAssistant, kText: reply}}})
	prof, err := os.MkdirTemp("", "pageact-profile-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(prof) })
	out, err := exec.CommandContext(t.Context(), node, filepath.Join("testdata", "pageact", "run.js"), bin, e.url, prof).CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
}

func TestPageSendNeedsUserActivationFirefox(t *testing.T) {
	t.Parallel()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	bin := firefoxPath(t)
	e := setup(t)
	reply := "Here is the page.\n\n```page\n" + activationPage + "\n```\n"
	e.gw.AddSession(&gatewaytest.Session{ID: "s9", Title: "Pages to click", Live: true, StartedAt: 900, Messages: []map[string]any{
		{kRole: roleUser, kText: "make the click page"}, {kRole: roleAssistant, kText: reply}}})
	prof, err := os.MkdirTemp("", "pageact-profile-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(prof) })
	out, err := exec.CommandContext(t.Context(), node, filepath.Join("testdata", "pageact", "run.js"), bin, e.url, prof).CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
}

// firefoxPath finds Firefox, takes a browser slot like chromePath, and skips the test where there is none.
func firefoxPath(t *testing.T) string {
	t.Helper()
	for _, n := range []string{"firefox", "firefox-esr"} {
		if p, err := exec.LookPath(n); err == nil {
			takeChromeSlot(t)
			return p
		}
	}
	t.Skip("Firefox is not installed")
	return ""
}
