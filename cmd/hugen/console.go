package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"

	adkagent "google.golang.org/adk/agent"
	"google.golang.org/adk/runner"
	adksession "google.golang.org/adk/session"
	"google.golang.org/genai"
)

// serveConsole launches a minimal stdin REPL for the agent.
//
// The auth mux is served on cfg.A2A.Port in the background so OIDC
// callbacks reach the registry even though no A2A endpoints are
// mounted.
func serveConsole(ctx context.Context, a *app) error {
	addr := fmt.Sprintf(":%d", a.cfg.A2A.Port)
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", addr, err)
	}
	srv := &http.Server{Handler: a.authMux}
	go func() {
		if err := srv.Serve(listener); err != nil && err != http.ErrServerClosed {
			a.logger.Error("auth callback server", "err", err)
		}
	}()
	go func() {
		<-ctx.Done()
		_ = srv.Close()
	}()
	for _, p := range a.prompts {
		go p()
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

	const userID = "console"
	sess, err := a.runtime.Sessions.Create(ctx, &adksession.CreateRequest{
		AppName: agentName(a.cfg),
		UserID:  userID,
	})
	if err != nil {
		return fmt.Errorf("session create: %w", err)
	}
	sessionID := sess.Session.ID()

	fmt.Fprintf(os.Stdout, "hugen console — model=%s session=%s. Type a message and press Enter (Ctrl-D to exit).\n",
		a.cfg.LLM.Model, sessionID)

	in := bufio.NewReader(os.Stdin)
	for {
		fmt.Fprint(os.Stdout, "> ")
		line, err := in.ReadString('\n')
		if err != nil {
			if err == io.EOF {
				fmt.Fprintln(os.Stdout)
				return nil
			}
			return fmt.Errorf("read stdin: %w", err)
		}
		prompt := strings.TrimSpace(line)
		if prompt == "" {
			continue
		}
		if err := streamReply(ctx, r, userID, sessionID, prompt, os.Stdout); err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
		}
	}
}

// streamReply sends a single user prompt to the agent and writes
// every assistant text delta to w. Returns the first stream error,
// or nil on a clean turn.
func streamReply(ctx context.Context, r *runner.Runner, userID, sessionID, prompt string, w io.Writer) error {
	msg := genai.NewContentFromText(prompt, genai.RoleUser)
	for ev, err := range r.Run(ctx, userID, sessionID, msg, adkagent.RunConfig{}) {
		if err != nil {
			fmt.Fprintln(w)
			return err
		}
		if ev == nil || ev.Content == nil {
			continue
		}
		for _, p := range ev.Content.Parts {
			if p == nil || p.Text == "" {
				continue
			}
			fmt.Fprint(w, p.Text)
		}
	}
	fmt.Fprintln(w)
	return nil
}
