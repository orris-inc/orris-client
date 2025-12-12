package agent

import (
	"context"
	"fmt"
	"sync"

	"github.com/orris-inc/orris-client/internal/forward"

	"github.com/orris-inc/orris-client/internal/api"
	"github.com/orris-inc/orris-client/internal/config"
	"github.com/orris-inc/orris-client/internal/forwarder"
	"github.com/orris-inc/orris-client/internal/status"
	"github.com/orris-inc/orris-client/internal/tunnel"
)

type Agent struct {
	cfg       *config.Config
	client    *api.Client
	collector *status.Collector

	forwardersMu sync.RWMutex
	forwarders   map[string]forwarder.Forwarder

	tunnelsMu sync.RWMutex
	tunnels   map[string]*tunnel.Client // ruleID -> tunnel

	tunnelServer  *tunnel.Server
	signingSecret string // also used as shared secret for key derivation
	clientToken   string // agent's own token for tunnel handshake (from API response)
	rules         []forward.Rule
	rulesMu       sync.RWMutex

	configVersion uint64 // current config version from server

	ctx      context.Context
	cancelFn context.CancelFunc
	wg       sync.WaitGroup
}

func New(cfg *config.Config) *Agent {
	client := api.NewClient(cfg.ServerURL, cfg.Token, cfg.HTTPTimeout)

	return &Agent{
		cfg:        cfg,
		client:     client,
		collector:  status.NewCollector(),
		forwarders: make(map[string]forwarder.Forwarder),
		tunnels:    make(map[string]*tunnel.Client),
	}
}

func (a *Agent) Start(ctx context.Context) error {
	a.ctx, a.cancelFn = context.WithCancel(ctx)

	if err := a.syncRules(); err != nil {
		return fmt.Errorf("initial sync failed: %w", err)
	}

	a.wg.Add(4)
	go a.syncLoop()
	go a.trafficLoop()
	go a.statusLoop()
	go a.hubLoop()

	return nil
}

func (a *Agent) Stop() {
	if a.cancelFn != nil {
		a.cancelFn()
	}

	a.reportFinalTraffic()
	a.stopAll()
	a.wg.Wait()
}

func (a *Agent) Wait() {
	a.wg.Wait()
}
