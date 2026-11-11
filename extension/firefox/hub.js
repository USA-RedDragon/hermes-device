"use strict";

// normalizeHubURL checks a hub address the user typed and returns it
// without a trailing slash. Only https is allowed, and plain http only for
// this computer, for tests. The rules match ValidHubURL in embed.go.
function normalizeHubURL(text) {
  const raw = String(text || "").trim();
  let u;
  try {
    u = new URL(raw);
  } catch (e) {
    throw new Error("That is not a web address. Enter it as https://hermes.example.com");
  }
  const local = u.hostname === "localhost" || u.hostname === "127.0.0.1" || u.hostname === "[::1]";
  if (u.protocol !== "https:" && !(u.protocol === "http:" && local)) {
    throw new Error("The hub address must start with https://");
  }
  if (u.username || u.password) {
    throw new Error("The hub address must not contain a user name or password.");
  }
  if (raw.includes("#") || raw.includes("?")) {
    throw new Error("The hub address must not contain # or ?.");
  }
  return u.origin + u.pathname.replace(/\/+$/, "");
}
