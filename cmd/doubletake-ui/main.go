package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"

	"doubletake/internal/daemon"
	"doubletake/internal/shell"
)

func main() {
	listen := flag.String("listen", "127.0.0.1:8199", "HTTP address for the frontend shell")
	socket := flag.String("socket", daemon.DefaultSocketPath(), "doubletake daemon socket path")
	uiDir := flag.String("ui-dir", "", "directory containing the built frontend (auto-detect when empty)")
	openBrowser := flag.Bool("open", true, "open the shell in the default browser")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := shell.Run(ctx, shell.Options{
		Listen:      *listen,
		SocketPath:  *socket,
		StaticDir:   *uiDir,
		OpenBrowser: *openBrowser,
	}); err != nil {
		log.Fatal(err)
	}
}
