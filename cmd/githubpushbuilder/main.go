// Command githubpushbuilder is a self-hosted build service: it receives GitHub
// push webhooks, runs the build script that lives in the pushed repo, and
// serves a dashboard for status, logs, artifacts and manual control.
package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/nsiva7/GithubPushBuilder/internal/config"
	"github.com/nsiva7/GithubPushBuilder/internal/engine"
	"github.com/nsiva7/GithubPushBuilder/internal/store"
	"github.com/nsiva7/GithubPushBuilder/internal/web"
	"github.com/nsiva7/GithubPushBuilder/internal/webhook"
)

func main() {
	log.SetFlags(log.LstdFlags | log.LUTC)
	cfg := config.FromEnv()

	cmd := "serve"
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}
	var err error
	switch cmd {
	case "serve":
		err = serve(cfg)
	case "set-password":
		err = setPassword(cfg)
	case "help", "-h", "--help":
		usage()
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		log.Fatalf("%s: %v", cmd, err)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `Usage: githubpushbuilder [command]

Commands:
  serve          Run the service (default)
  set-password   Set a new admin password, read from standard input

Environment:
  GPB_DATA_DIR         Data directory (default /data)
  GPB_LISTEN           Listen address (default :8080)
  GPB_BASE_URL         Public URL, e.g. https://builds.example.com
  GPB_ADMIN_PASSWORD   Admin password for the first start (later changes: dashboard or set-password)
  GPB_TRUST_PROXY      1 when behind a reverse proxy that sets X-Forwarded-For
  GPB_INSECURE_COOKIE  1 to allow the session cookie over plain http (local testing only)
  GPB_REPO_SCRIPT      Build script path inside a repo (default `+config.DefaultRepoScript+`)
  GPB_GITHUB_API_URL   GitHub API base (default https://api.github.com)
`)
}

func openStore(cfg config.Config) (*store.Store, error) {
	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		return nil, err
	}
	return store.Open(filepath.Join(cfg.DataDir, "db.sqlite"))
}

func serve(cfg config.Config) error {
	st, err := openStore(cfg)
	if err != nil {
		return err
	}
	defer st.Close()

	generated, err := web.EnsureAdminPassword(st, cfg.AdminPassword)
	if err != nil {
		return fmt.Errorf("admin password: %w", err)
	}
	if generated != "" {
		log.Printf("No admin password was set. Generated one — sign in with it and change it under Settings:")
		log.Printf("    %s", generated)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	eng := engine.New(cfg, st)
	if err := eng.Start(ctx); err != nil {
		return err
	}
	handler, err := web.New(cfg, st, eng, &webhook.Handler{Store: st, Queue: eng})
	if err != nil {
		return err
	}
	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		// No write timeout: log streams and artifact downloads are long-lived.
	}

	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	log.Printf("%s listening on %s, data in %s", config.ServiceName, cfg.Listen, cfg.DataDir)

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	log.Printf("shutting down; running builds are stopped and re-queued on the next start")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		srv.Close() // open log streams don't end on their own
	}
	eng.Wait()
	return nil
}

func setPassword(cfg config.Config) error {
	st, err := openStore(cfg)
	if err != nil {
		return err
	}
	defer st.Close()
	fmt.Fprint(os.Stderr, "New admin password: ")
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && line == "" {
		return errors.New("no password given")
	}
	if err := web.SetPassword(st, strings.TrimRight(line, "\r\n")); err != nil {
		return err
	}
	if err := st.DeleteSessionsExcept(""); err != nil {
		return err
	}
	fmt.Fprintln(os.Stderr, "Password changed. All sessions were signed out.")
	return nil
}
