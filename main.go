// Forge is a local, single-binary AI coding agent harness.
package main

import (
	"context"
	"embed"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"forge/internal/config"
	"forge/internal/logx"
	"forge/internal/server"
	"forge/internal/store"
)

//go:embed web
var webFiles embed.FS

var version = "dev"

func main() {
	var (
		host      string
		port      int
		noBrowser bool
		debug     bool
		debugLLM  bool
		logFile   string
		showVer   bool
	)
	flag.StringVar(&host, "host", "", "override the configured listen host")
	flag.IntVar(&port, "port", 0, "override the configured HTTP port")
	flag.BoolVar(&noBrowser, "no-browser", false, "do not open the browser on startup")
	flag.BoolVar(&debug, "debug", false, "enable verbose DEBUG logging")
	flag.BoolVar(&debugLLM, "debug-llm", false, "log every LLM request and response payload in full (implies --debug)")
	flag.StringVar(&logFile, "log-file", "", "write logs to this file (default: <forge home>/forge.log)")
	flag.BoolVar(&showVer, "version", false, "print the version and exit")
	flag.Parse()

	if showVer {
		fmt.Println("forge", version)
		return
	}

	cfgMgr, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "forge: %v\n", err)
		os.Exit(1)
	}
	cfg := cfgMgr.Get()

	envDebug := envBool("FORGE_DEBUG")
	envLLM := envBool("FORGE_LLM_DEBUG") || envBool("FORGE_DEBUG_LLM")
	debugLLM = debugLLM || envLLM || cfg.Server.DebugLLM
	// The trace level sits below DEBUG, so a trace session always keeps the
	// round/session metadata that makes the dumps readable.
	debug = debug || envDebug || cfg.Server.Debug || debugLLM
	debugForced := debug && (flagWasSet("debug") || envDebug)
	llmForced := debugLLM && (flagWasSet("debug-llm") || envLLM)

	path := logFile
	if path == "" {
		path, err = config.LogPath(cfg)
		if err != nil {
			fmt.Fprintf(os.Stderr, "forge: resolve log path: %v\n", err)
			path = ""
		}
	}
	if err := logx.Init(logx.Options{
		Debug:      debug,
		ForceDebug: debugForced,
		LLMDebug:   debugLLM,
		ForceLLM:   llmForced,
		FilePath:   path,
	}); err != nil {
		fmt.Fprintf(os.Stderr, "forge: %v (continuing with stderr only)\n", err)
	}
	defer logx.Close()

	logx.Info("starting forge", "version", version, "debug", logx.DebugEnabled(),
		"llm_trace", logx.LLMEnabled(), "log", path)
	if logx.LLMEnabled() {
		logx.Warn("LLM payload tracing is on: prompts, tool output and responses are written to the log")
	}

	if host == "" {
		host = cfg.Server.Host
	}
	if port == 0 {
		port = cfg.Server.Port
	}
	if port < 1 || port > 65535 {
		logx.Fatalf("invalid port %d", port)
	}

	dbPath, err := config.DBPath()
	if err != nil {
		logx.Fatalf("%v", err)
	}
	st, err := store.Open(dbPath)
	if err != nil {
		logx.Fatalf("open database: %v", err)
	}
	defer st.Close()
	if st.LegacyBackupPath != "" {
		logx.Warn("incompatible legacy database moved aside", "backup", st.LegacyBackupPath)
	}

	webRoot, err := fs.Sub(webFiles, "web")
	if err != nil {
		logx.Fatalf("embedded web assets: %v", err)
	}
	handler := server.New(cfgMgr, st, webRoot).Handler()

	addr := net.JoinHostPort(host, strconv.Itoa(port))
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		logx.Fatalf("listen on %s: %v", addr, err)
	}

	displayHost := host
	if host == "0.0.0.0" || host == "::" || host == "" {
		displayHost = "127.0.0.1"
	}
	url := "http://" + net.JoinHostPort(displayHost, strconv.Itoa(port))
	logx.Info("forge ready", "url", url, "config", cfgMgr.Path(), "db", dbPath)
	if !isLoopback(host) {
		logx.Warn("bound to a non-loopback address with no authentication; only expose Forge on trusted networks", "host", host)
	}

	srv := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	if cfg.Server.AutoOpenBrowser && !noBrowser {
		if isLoopback(host) {
			go func() {
				time.Sleep(350 * time.Millisecond)
				if err := openBrowser(url); err != nil {
					logx.Warn("could not open browser", "error", err)
				}
			}()
		}
	}

	errCh := make(chan error, 1)
	go func() {
		errCh <- srv.Serve(ln)
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	select {
	case sig := <-sigCh:
		logx.Info("shutting down", "signal", sig.String())
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			logx.Error("shutdown failed", "error", err)
		}
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			logx.Fatalf("%v", err)
		}
	}
}

func isLoopback(host string) bool {
	h := strings.TrimSpace(host)
	return h == "" || h == "localhost" || h == "127.0.0.1" || h == "::1" || strings.HasPrefix(h, "127.")
}

func envBool(name string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(name))) {
	case "1", "true", "yes", "on", "debug":
		return true
	default:
		return false
	}
}

// flagWasSet reports whether the named flag was passed explicitly.
func flagWasSet(name string) bool {
	set := false
	flag.Visit(func(f *flag.Flag) {
		if f.Name == name {
			set = true
		}
	})
	return set
}

func openBrowser(url string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	return cmd.Start()
}
