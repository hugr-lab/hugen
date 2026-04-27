package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	adkagent "google.golang.org/adk/agent"
	"google.golang.org/adk/runner"
	adksession "google.golang.org/adk/session"
	"google.golang.org/genai"

	"github.com/hugr-lab/hugen/pkg/devui"
)

// serveWebUI launches a loopback HTTP chat UI alongside the auth /
// callback mux. No A2A endpoints are mounted — the webui mode is
// developer-only.
func serveWebUI(ctx context.Context, a *app) error {
	authSrv := &http.Server{
		Addr:    fmt.Sprintf(":%d", a.cfg.A2A.Port),
		Handler: a.authMux,
	}

	r, err := runner.New(runner.Config{
		AppName:           agentName(a.cfg),
		Agent:             a.runtime.Agent,
		SessionService:    a.runtime.Sessions,
		AutoCreateSession: true,
	})
	if err != nil {
		return fmt.Errorf("runner: %w", err)
	}

	devMux := http.NewServeMux()
	devMux.HandleFunc("/", indexHandler(a))
	devMux.HandleFunc("/chat", chatHandler(a, r))
	devMux.Handle("/dev/token", devui.TokenHandler(a.authReg.TokenStores()))
	devMux.Handle("/dev/auth/trigger", devui.TriggerAuthHandler(a.cfg.A2A.BaseURL))

	devSrv := &http.Server{
		Addr:    fmt.Sprintf("127.0.0.1:%d", a.cfg.DevUI.Port),
		Handler: devMux,
	}

	a.logger.Info("webui: A2A auth listener",
		"addr", authSrv.Addr,
		"callback", a.cfg.A2A.BaseURL+"/auth/callback")
	a.logger.Info("webui: chat listener (loopback only)",
		"addr", devSrv.Addr,
		"ui", a.cfg.DevUI.BaseURL+"/")

	return serve(ctx, []*http.Server{authSrv, devSrv}, a.prompts, a.logger)
}

// indexHandler serves a tiny HTML chat page. The page POSTs to /chat
// and renders the streamed text response inline.
func indexHandler(a *app) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprintf(w, indexHTML, a.cfg.LLM.Model)
	}
}

// chatHandler accepts a JSON {prompt, session_id?} body and streams
// the agent's text response back via Server-Sent Events. Each `data:`
// line is a partial assistant text delta; the stream ends with an
// `event: done` line carrying the (possibly newly created) session id.
func chatHandler(a *app, r *runner.Runner) http.HandlerFunc {
	type req struct {
		Prompt    string `json:"prompt"`
		SessionID string `json:"session_id"`
	}
	return func(w http.ResponseWriter, httpReq *http.Request) {
		if httpReq.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var body req
		if err := json.NewDecoder(httpReq.Body).Decode(&body); err != nil {
			http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
			return
		}
		if body.Prompt == "" {
			http.Error(w, "missing prompt", http.StatusBadRequest)
			return
		}

		sessionID := body.SessionID
		if sessionID == "" {
			sess, err := a.runtime.Sessions.Create(httpReq.Context(), &adksession.CreateRequest{
				AppName: agentName(a.cfg),
				UserID:  "webui",
			})
			if err != nil {
				http.Error(w, "session create: "+err.Error(), http.StatusInternalServerError)
				return
			}
			sessionID = sess.Session.ID()
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		flusher, _ := w.(http.Flusher)

		msg := genai.NewContentFromText(body.Prompt, genai.RoleUser)
		for ev, err := range r.Run(httpReq.Context(), "webui", sessionID, msg, adkagent.RunConfig{}) {
			if err != nil {
				fmt.Fprintf(w, "event: error\ndata: %s\n\n", jsonString(err.Error()))
				if flusher != nil {
					flusher.Flush()
				}
				return
			}
			if ev == nil || ev.Content == nil {
				continue
			}
			for _, p := range ev.Content.Parts {
				if p == nil || p.Text == "" {
					continue
				}
				fmt.Fprintf(w, "data: %s\n\n", jsonString(p.Text))
				if flusher != nil {
					flusher.Flush()
				}
			}
		}
		fmt.Fprintf(w, "event: done\ndata: %s\n\n", jsonString(sessionID))
		if flusher != nil {
			flusher.Flush()
		}
	}
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

const indexHTML = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<title>hugen — chat</title>
<style>
  body { font-family: ui-monospace, SFMono-Regular, monospace; max-width: 720px; margin: 24px auto; padding: 0 16px; }
  h1 { font-size: 1.1rem; }
  #log { white-space: pre-wrap; border: 1px solid #ccc; padding: 12px; min-height: 360px; max-height: 60vh; overflow: auto; }
  textarea { width: 100%%; min-height: 80px; box-sizing: border-box; }
  .role { color: #888; font-weight: bold; }
</style>
</head>
<body>
<h1>hugen — model: %s</h1>
<div id="log"></div>
<form id="f">
  <textarea id="p" placeholder="ask anything..."></textarea>
  <button type="submit">send</button>
</form>
<script>
let sid = "";
const log = document.getElementById("log");
const form = document.getElementById("f");
const input = document.getElementById("p");

function append(role, text) {
  const span = document.createElement("span");
  span.className = "role";
  span.textContent = role + ": ";
  log.appendChild(span);
  log.appendChild(document.createTextNode(text + "\n\n"));
  log.scrollTop = log.scrollHeight;
}

form.addEventListener("submit", async (e) => {
  e.preventDefault();
  const prompt = input.value.trim();
  if (!prompt) return;
  append("you", prompt);
  input.value = "";

  const resp = await fetch("/chat", {
    method: "POST",
    headers: {"Content-Type": "application/json"},
    body: JSON.stringify({prompt, session_id: sid}),
  });
  if (!resp.ok || !resp.body) {
    append("error", await resp.text());
    return;
  }

  const reader = resp.body.getReader();
  const dec = new TextDecoder();
  let buf = "";
  let printedHeader = false;
  let outNode = null;

  while (true) {
    const {value, done} = await reader.read();
    if (done) break;
    buf += dec.decode(value, {stream: true});
    const lines = buf.split("\n");
    buf = lines.pop() || "";
    let event = "message";
    for (const line of lines) {
      if (line.startsWith("event: ")) {
        event = line.slice(7).trim();
        continue;
      }
      if (!line.startsWith("data: ")) continue;
      const data = JSON.parse(line.slice(6));
      if (event === "done") {
        sid = data;
        log.appendChild(document.createTextNode("\n"));
        log.scrollTop = log.scrollHeight;
        return;
      }
      if (event === "error") {
        append("error", data);
        return;
      }
      if (!printedHeader) {
        const span = document.createElement("span");
        span.className = "role";
        span.textContent = "agent: ";
        log.appendChild(span);
        outNode = document.createTextNode("");
        log.appendChild(outNode);
        printedHeader = true;
      }
      outNode.data += data;
      log.scrollTop = log.scrollHeight;
    }
  }
});
</script>
</body>
</html>
`
