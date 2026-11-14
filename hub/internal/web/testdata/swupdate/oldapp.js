// The previous release's script, as a browser has it cached.
window.__release = "old";
if ("serviceWorker" in navigator) navigator.serviceWorker.register("/sw.js");
