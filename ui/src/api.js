export const defaultSettings = {
  screen: "auto",
  virtualPosition: "right",
  fps: 60,
  bitrate: 0,
  targetLatencyMs: 100,
  hwaccel: "auto",
  noAudio: false,
  port: 7000,
  portMin: 0,
  portMax: 0,
  socketPath: "",
  credBackend: "keyring",
  credFile: "~/.config/doubletake/credentials.json",
  forcePair: false,
  debug: false,
  testMode: false,
  noEncrypt: false,
  directKey: false,
};

async function request(path, options) {
  const response = await fetch(path, { headers: { "Content-Type": "application/json" }, ...options });
  const body = await response.json().catch(() => ({}));
  if (!response.ok) throw new Error(body.error || `Request failed (${response.status})`);
  return body;
}

export const api = {
  snapshot: () => request("/api/snapshot"),
  settings: () => request("/api/settings"),
  saveSettings: (settings) => request("/api/settings", { method: "PUT", body: JSON.stringify(settings) }),
  action: (action, target) => request(`/api/actions/${action}`, { method: "POST", body: JSON.stringify({ target }) }),
};
