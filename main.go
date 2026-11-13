// Forge is a local, single-binary AI coding agent harness.
package main

import (
	"context"
	"embed"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log"
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
		showVer   bool
	)
	flag.StringVar(&host, "host", "", "override the configured listen host")
	flag.IntVar(&port, "port", 0, "override the configured HTTP port")
	flag.BoolVar(&noBrowser, "no-browser", false, "do not open the browser on startup")
	flag.BoolVar(&showVer, "version", false, "print the version and exit")
	flag.Parse()

	if showVer {
		fmt.Println("forge", version)
		return
	}

	log.SetFlags(log.Ltime)
	cfgMgr, err := config.Load()
	if err != nil {
		log.Fatalf("forge: %v", err)
	}
	cfg := cfgMgr.Get()
	if host == "" {
		host = cfg.Server.Host
	}
	if port == 0 {
		port = cfg.Server.Port
	}
	if port < 1 || port > 65535 {
		log.Fatalf("forge: invalid port %d", port)
	}

	dbPath, err := config.DBPath()
	if err != nil {
		log.Fatalf("forge: %v", err)
	}
	st, err := store.Open(dbPath)
	if err != nil {
		log.Fatalf("forge: open database: %v", err)
	}
	defer st.Close()
	if st.LegacyBackupPath != "" {
		log.Printf("incompatible legacy database moved to %s", st.LegacyBackupPath)
	}

	webRoot, err := fs.Sub(webFiles, "web")
	if err != nil {
		log.Fatalf("forge: embedded web assets: %v", err)
	}
	handler := server.New(cfgMgr, st, webRoot).Handler()

	addr := net.JoinHostPort(host, strconv.Itoa(port))
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		log.Fatalf("forge: listen on %s: %v", addr, err)
	}

	displayHost := host
	if host == "0.0.0.0" || host == "::" || host == "" {
		displayHost = "127.0.0.1"
	}
	url := "http://" + net.JoinHostPort(displayHost, strconv.Itoa(port))
	log.Printf("Forge %s ready at %s", version, url)
	log.Printf("data: %s", dbPath)
	if !isLoopback(host) {
		log.Printf("warning: Forge is bound to %s with no authentication; only expose it on trusted networks", host)
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
					log.Printf("could not open browser: %v", err)
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
		log.Printf("received %s, shutting down", sig)
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			log.Printf("shutdown: %v", err)
		}
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("forge: %v", err)
		}
	}
}

func isLoopback(host string) bool {
	h := strings.TrimSpace(host)
	return h == "" || h == "localhost" || h == "127.0.0.1" || h == "::1" || strings.HasPrefix(h, "127.")
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
