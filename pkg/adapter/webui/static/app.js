// hugen webui — single-page chat UI driving /api/v1.
//
// Lifecycle:
//   1. Fetch /api/auth/dev-token (loopback bypass on the API listener).
//   2. Pick up an existing ?session=<id> from the URL or open a new one.
//   3. Open EventSource on /api/v1/sessions/<id>/stream with the
//      stored Last-Event-ID; update it on every event.
//   4. Section-state-machine renders chunked reasoning/agent_message
//      so streaming chunks accumulate into a single visible section
//      until "final" flips, at which point the next chunk opens a
//      fresh section.
(function () {
  "use strict";

  const apiOrigin = (() => {
    // The webui listener is loopback-only; the API listener may be
    // on a different port. We look up the running API base from a
    // <meta name="hugen-api"> tag if present, else assume same-host
    // on port HUGEN_PORT exposed via the dev-token endpoint.
    const meta = document.querySelector('meta[name="hugen-api"]');
    if (meta && meta.content) return meta.content.replace(/\/+$/, "");
    // Default: same hostname, port = location.port - 1 is brittle;
    // require the operator to set <meta> in production. For loopback
    // dev convenience, allow the URL ?api=... override.
    const params = new URLSearchParams(location.search);
    if (params.get("api")) return params.get("api").replace(/\/+$/, "");
    // Fallback: assume the API is exposed on the standard hugen port
    // (HUGEN_PORT) at 127.0.0.1. Operators with non-default port
    // layouts must pass ?api=...
    return "http://127.0.0.1:10000";
  })();

  const $log = document.getElementById("log");
  const $session = document.getElementById("session");
  const $input = document.getElementById("input");
  const $send = document.getElementById("send");
  const $new = document.getElementById("newSession");

  let token = null;
  let sessionID = null;
  let es = null;
  let activeSection = null; // { kind, el, final }

  function lastEventKey(id) { return "hugen.lastEventId." + id; }

  function appendSection(kind) {
    const el = document.createElement("div");
    el.className = "section " + kind;
    $log.appendChild(el);
    el.scrollIntoView({ behavior: "smooth", block: "end" });
    return el;
  }

  function ensureSection(kind) {
    if (!activeSection || activeSection.kind !== kind || activeSection.final) {
      activeSection = { kind, el: appendSection(kind), final: false };
    }
    return activeSection;
  }

  function flushSection() { activeSection = null; }

  function renderFrame(kind, frame) {
    const payload = frame.payload || {};
    if (kind === "user_message") {
      flushSection();
      const sec = appendSection("user-message");
      sec.textContent = payload.text || "";
      return;
    }
    if (kind === "agent_message" || kind === "reasoning") {
      const sec = ensureSection(kind === "reasoning" ? "reasoning" : "agent-message");
      if (payload.text) sec.el.textContent += payload.text;
      if (payload.final) sec.final = true;
      return;
    }
    if (kind === "session_opened") {
      flushSection();
      const sec = appendSection("system");
      sec.textContent = "session opened";
      return;
    }
    if (kind === "session_closed") {
      flushSection();
      const sec = appendSection("system");
      sec.textContent = "session closed: " + (payload.reason || "");
      if (es) { es.close(); es = null; }
      return;
    }
    if (kind === "session_suspended") {
      flushSection();
      const sec = appendSection("system");
      sec.textContent = "session suspended";
      return;
    }
    if (kind === "error") {
      flushSection();
      const sec = appendSection("error");
      sec.textContent = "error: " + (payload.message || payload.code || "");
      return;
    }
    if (kind === "system_marker" || kind === "slash_command" ||
        kind === "tool_call" || kind === "tool_result") {
      flushSection();
      const sec = appendSection("system");
      sec.textContent = kind + ": " + JSON.stringify(payload);
      return;
    }
    // unknown / opaque variants — surface them so we don't lose visibility.
    flushSection();
    const sec = appendSection("system");
    sec.textContent = kind + ": " + JSON.stringify(payload);
  }

  async function fetchDevToken() {
    // credentials: "include" — accept the Set-Cookie response so the
    // EventSource handshake can carry it.
    const r = await fetch(apiOrigin + "/api/auth/dev-token", { credentials: "include" });
    if (!r.ok) throw new Error("dev-token fetch failed: " + r.status);
    const body = await r.json();
    return body.token;
  }

  async function openSession() {
    const r = await fetch(apiOrigin + "/api/v1/sessions", {
      method: "POST",
      headers: { "Authorization": "Bearer " + token, "Content-Type": "application/json" },
      body: JSON.stringify({})
    });
    if (!r.ok) throw new Error("open session failed: " + r.status);
    const body = await r.json();
    return body.session_id;
  }

  function attachStream(id) {
    if (es) { es.close(); es = null; }
    const lastID = localStorage.getItem(lastEventKey(id));
    const url = new URL(apiOrigin + "/api/v1/sessions/" + id + "/stream");
    if (lastID) url.searchParams.set("last_event_id", lastID);
    // EventSource cannot set headers; the auth gate falls back to the
    // hugen_dev_token cookie set by /api/auth/dev-token. Cross-origin
    // cookies require withCredentials.
    es = new EventSource(url.toString(), { withCredentials: true });
    es.onerror = () => {
      // EventSource auto-reconnects with the original Last-Event-ID
      // header; our manual store keeps it across full reloads.
    };
    const variants = [
      "user_message", "agent_message", "reasoning", "slash_command",
      "cancel", "session_opened", "session_closed", "session_suspended",
      "error", "system_marker", "tool_call", "tool_result"
    ];
    for (const k of variants) {
      es.addEventListener(k, (ev) => {
        try {
          const frame = JSON.parse(ev.data);
          renderFrame(k, frame);
          if (ev.lastEventId) localStorage.setItem(lastEventKey(id), ev.lastEventId);
        } catch (e) {
          console.error("frame parse failed", e, ev.data);
        }
      });
    }
  }

  async function sendInput() {
    const text = $input.value;
    if (!text || !sessionID) return;
    $input.value = "";
    const isCmd = text.startsWith("/");
    const body = isCmd
      ? { kind: "slash_command", author: { id: "user", kind: "user" }, payload: { raw: text } }
      : { kind: "user_message", author: { id: "user", kind: "user" }, payload: { text } };
    const r = await fetch(apiOrigin + "/api/v1/sessions/" + sessionID + "/post", {
      method: "POST",
      headers: { "Authorization": "Bearer " + token, "Content-Type": "application/json" },
      body: JSON.stringify(body)
    });
    if (!r.ok) {
      const err = await r.text();
      console.error("post failed", err);
    }
  }

  async function newSession() {
    sessionID = await openSession();
    $session.textContent = sessionID;
    history.replaceState(null, "", "?session=" + encodeURIComponent(sessionID));
    localStorage.removeItem(lastEventKey(sessionID));
    $log.innerHTML = "";
    flushSection();
    attachStream(sessionID);
  }

  async function bootstrap() {
    token = await fetchDevToken();
    const params = new URLSearchParams(location.search);
    const existing = params.get("session");
    if (existing) {
      sessionID = existing;
      $session.textContent = sessionID;
      attachStream(sessionID);
    } else {
      await newSession();
    }
  }

  $send.addEventListener("click", sendInput);
  $input.addEventListener("keydown", (e) => {
    if (e.key === "Enter" && !e.shiftKey) {
      e.preventDefault();
      sendInput();
    }
  });
  $new.addEventListener("click", () => { newSession(); });

  bootstrap().catch((e) => {
    const sec = appendSection("error");
    sec.textContent = "bootstrap failed: " + e.message;
    console.error(e);
  });
})();
