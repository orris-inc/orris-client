package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/easayliu/orris-client/internal/agent"
	"github.com/easayliu/orris-client/internal/config"
	"github.com/easayliu/orris-client/internal/logger"
	flag "github.com/spf13/pflag"
)

func main() {
	var (
		serverURL    = flag.StringP("server", "u", "", "server URL")
		token        = flag.StringP("token", "t", "", "bearer token")
		wsListenPort = flag.Uint16P("ws-port", "w", 0, "WebSocket listen port for tunnel (0 = random)")
	)
	flag.Parse()

	cfg := config.LoadFromEnv()

	if *serverURL != "" {
		cfg.ServerURL = *serverURL
	}
	if *token != "" {
		cfg.Token = *token
	}
	if *wsListenPort != 0 {
		cfg.WsListenPort = *wsListenPort
	}

	if cfg.Token == "" {
		logger.Error("token is required (use --token or ORRIS_TOKEN env)")
		os.Exit(1)
	}

	logger.Info("starting orris-agent", "server", cfg.ServerURL)

	ag := agent.New(cfg)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := ag.Start(ctx); err != nil {
		logger.Error("failed to start agent", "error", err)
		os.Exit(1)
	}

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	<-sigChan
	logger.Info("shutting down")

	ag.Stop()
	logger.Info("agent stopped")
}
