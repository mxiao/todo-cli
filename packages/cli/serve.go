package cli

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"strconv"
	"syscall"

	"github.com/mxiao/todo-cli/packages/server"
)

// EnvPort overrides the default web service port.
const EnvPort = "TODO_CLI_PORT"

// serveInfo is what `todo serve --json` prints once it is listening.
type serveInfo struct {
	URL           string `json:"url"`
	Host          string `json:"host"`
	Port          int    `json:"port"`
	RequestedPort int    `json:"requested_port"`
	DataDir       string `json:"data_dir"`
	PID           int    `json:"pid"`
}

func cmdServe(a *app, args []string) error {
	fs := newFlags("serve")
	defPort := server.DefaultPort
	if v := a.env.Getenv(EnvPort); v != "" {
		p, err := strconv.Atoi(v)
		if err != nil {
			return usagef("%s=%q is not a port number", EnvPort, v)
		}
		defPort = p
	}
	port := fs.Int("port", defPort, "first port to try; the next ones are tried while it is in use (also $"+EnvPort+")")
	host := fs.String("host", server.DefaultHost, "loopback address to listen on (127.0.0.1, ::1 or localhost)")
	open := fs.Bool("open", false, "open the web page in the default browser")
	rest, err := a.parse(fs, args)
	if err != nil {
		return err
	}
	if len(rest) > 0 {
		return usagef("serve takes no arguments")
	}
	if _, err := server.CheckHost(*host); err != nil {
		return usagef("%v", err)
	}
	if *port < 0 || *port > 65535 {
		return usagef("--port %d is out of range", *port)
	}
	if a.actor == "" {
		a.actor = "web" // history records changes made through the web service as "web"
	}
	s, err := a.open()
	if err != nil {
		return err
	}
	ln, err := server.Listen(*host, *port, server.DefaultPortAttempts)
	if err != nil {
		return err
	}
	url := server.URL(ln)
	addr := ln.Addr().(*net.TCPAddr)
	if a.json {
		_ = a.emitJSON(serveInfo{URL: url, Host: addr.IP.String(), Port: addr.Port, RequestedPort: *port,
			DataDir: s.DataDir(), PID: os.Getpid()})
	} else {
		a.printf("todo-cli web: %s\n", url)
		if *port != 0 && addr.Port != *port {
			a.printf("  (port %d is in use; using %d)\n", *port, addr.Port)
		}
		a.printf("  data: %s\n  Press Ctrl+C to stop.\n", s.DBPath())
	}
	if *open {
		if err := openBrowser(url); err != nil {
			fmt.Fprintf(a.env.Stderr, "todo: could not open a browser (%v); open %s manually\n", err, url)
		}
	}

	ctx := a.env.Ctx
	if ctx == nil {
		var stop context.CancelFunc
		ctx, stop = signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
	}
	log := slog.New(slog.NewTextHandler(a.env.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	srv := server.New(s, server.Options{Logger: log, Version: Version})
	return srv.Serve(ctx, ln)
}

func openBrowser(url string) error {
	name := "xdg-open"
	if runtime.GOOS == "darwin" {
		name = "open"
	}
	return exec.Command(name, url).Start()
}
