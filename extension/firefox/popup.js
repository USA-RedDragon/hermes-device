"use strict";

const $ = (id) => document.getElementById(id);

function show(id, on) {
  $(id).hidden = !on;
}

function render(res) {
  const st = res.state;
  let status;
  if (!st.connected) {
    status = st.replaced ? "Another Firefox window took over browser control." : "hermes-device is not running on this computer.";
  } else if (!st.signed_in) {
    status = "Firefox is not signed in to Hermes. Sign in so your Hermes agent can use this browser.";
  } else if (st.auth_state === "signed_in") {
    status = "Signed in as " + st.auth_user + " for " + (st.device_name || st.token_device) + ".";
  } else if (st.auth_state === "refused" || st.auth_state === "offline") {
    status = st.auth_reason;
  } else {
    status = "Signed in as " + st.user + "; the hub is checking the sign-in.";
  }
  $("status").textContent = st.signing_in ? "Finish signing in in the window that opened." : status;
  $("hub-text").textContent = st.hub_url ? "Hub: " + st.hub_url : "No hub address is set.";
  $("change").textContent = st.hub_url ? "Change" : "Set the hub address";
  const hint = st.daemon_hub_url && st.daemon_hub_url !== st.hub_url;
  $("hint").textContent = hint ? "hermes-device on this computer uses " + st.daemon_hub_url + "." : "";
  show("hint", hint);
  const err = res.ok === false ? res.error : st.error;
  $("error").textContent = err || "";
  show("error", Boolean(err));
  show("sign-in", st.connected && !st.signing_in && (!st.signed_in || st.auth_state === "refused"));
  $("sign-in").textContent = st.signed_in ? "Sign in again" : "Sign in to Hermes";
  show("sign-out", st.signed_in);
  show("use", !st.connected && st.replaced);
}

async function command(cmd, arg) {
  render(await browser.runtime.sendMessage({ cmd, arg }));
}

$("sign-in").addEventListener("click", () => command("signIn"));
$("sign-out").addEventListener("click", () => command("signOut"));
$("use").addEventListener("click", () => command("connect"));
$("change").addEventListener("click", (e) => {
  e.preventDefault();
  browser.runtime.openOptionsPage();
  window.close();
});
command("state");
setInterval(() => command("state"), 2000);
