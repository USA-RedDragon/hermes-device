"use strict";

const $ = (id) => document.getElementById(id);

function render(res, keepInput) {
  const st = res.state;
  if (!keepInput) {
    $("url").value = st.hub_url;
  }
  const err = res.ok === false ? res.error : "";
  $("error").textContent = err;
  $("error").hidden = !err;
  const other = st.daemon_hub_url && st.daemon_hub_url !== st.hub_url;
  $("use-daemon").textContent = "Use " + st.daemon_hub_url + ", the hub of hermes-device on this computer";
  $("use-daemon").hidden = !other;
  $("who").textContent = st.signed_in ? "Signed in to " + st.token_hub + " as " + st.user + "." : "Firefox is not signed in to Hermes.";
  $("sign-out").hidden = !st.signed_in;
}

async function command(cmd, arg, keepInput) {
  const res = await browser.runtime.sendMessage({ cmd, arg });
  render(res, keepInput);
  return res;
}

async function saveURL(text) {
  $("saved").hidden = true;
  let url;
  try {
    url = text.trim() ? normalizeHubURL(text) : "";
  } catch (e) {
    $("error").textContent = e.message;
    $("error").hidden = false;
    return;
  }
  const res = await command("setHubURL", url, false);
  $("saved").hidden = res.ok === false;
}

$("form").addEventListener("submit", (e) => {
  e.preventDefault();
  saveURL($("url").value);
});
$("use-daemon").addEventListener("click", async () => {
  const res = await browser.runtime.sendMessage({ cmd: "state" });
  saveURL(res.state.daemon_hub_url);
});
$("sign-out").addEventListener("click", () => command("signOut", undefined, true));
command("state");
