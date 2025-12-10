package agent

import (
	"context"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/orris-inc/orris/sdk/forward"

	"github.com/easayliu/orris-client/internal/api"
	"github.com/easayliu/orris-client/internal/config"
	"github.com/easayliu/orris-client/internal/forwarder"
	"github.com/easayliu/orris-client/internal/logger"
	"github.com/easayliu/orris-client/internal/status"
	"github.com/easayliu/orris-client/internal/tunnel"
)

type Agent struct {
	cfg       *config.Config
	client    *api.Client
	collector *status.Collector

	forwardersMu sync.RWMutex
	forwarders   map[string]forwarder.Forwarder

	tunnelsMu sync.RWMutex
	tunnels   map[string]*tunnel.Client // exitAgentID -> tunnel

	tunnelServer *tunnel.Server

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

func (a *Agent) syncLoop() {
	defer a.wg.Done()

	ticker := time.NewTicker(a.cfg.SyncInterval)
	defer ticker.Stop()

	for {
		select {
		case <-a.ctx.Done():
			return
		case <-ticker.C:
			if err := a.syncRules(); err != nil {
				logger.Error("sync rules failed", "error", err)
			}
		}
	}
}

func (a *Agent) syncRules() error {
	logger.Info("requesting enabled rules")
	rules, err := a.client.GetRules(a.ctx)
	if err != nil {
		return err
	}

	logger.Info("rules synced successfully", "count", len(rules))

	ruleMap := make(map[string]*forward.Rule)
	for i := range rules {
		ruleMap[rules[i].ID] = &rules[i]
	}

	// Stop forwarders for removed rules
	a.forwardersMu.Lock()
	for ruleID, f := range a.forwarders {
		if _, exists := ruleMap[ruleID]; !exists {
			logger.Info("stopping forwarder for removed rule", "rule_id", ruleID)
			f.Stop()
			delete(a.forwarders, ruleID)
		}
	}
	a.forwardersMu.Unlock()

	// Start forwarders for new rules
	for _, rule := range rules {
		a.forwardersMu.RLock()
		_, exists := a.forwarders[rule.ID]
		a.forwardersMu.RUnlock()

		if !exists {
			r := rule
			if err := a.startForwarder(&r); err != nil {
				logger.Error("start forwarder failed", "rule_id", rule.ID, "error", err)
			}
		}
	}

	return nil
}

func (a *Agent) startForwarder(rule *forward.Rule) error {
	var f forwarder.Forwarder

	switch rule.RuleType {
	case forward.RuleTypeDirect:
		df := forwarder.NewDirectForwarder(rule)
		if err := df.Start(a.ctx); err != nil {
			return err
		}
		f = df

	case forward.RuleTypeEntry:
		// Handle based on agent's role in this rule
		switch rule.Role {
		case "entry":
			// Entry role: establish tunnel to exit agent
			t, err := a.getOrCreateTunnel(rule.ExitAgentID)
			if err != nil {
				return fmt.Errorf("create tunnel: %w", err)
			}

			ef := forwarder.NewEntryForwarder(rule, t)
			t.SetHandler(ef)
			if err := ef.Start(a.ctx); err != nil {
				return err
			}
			f = ef

		case "exit":
			// Exit role: accept tunnel connections and forward to target
			if err := a.ensureTunnelServer(); err != nil {
				return err
			}

			ef := forwarder.NewExitForwarder(rule)
			a.tunnelServer.AddHandler(rule.ID, ef)
			if err := ef.Start(a.ctx); err != nil {
				return err
			}
			f = ef

		default:
			return fmt.Errorf("unknown role %q for entry rule", rule.Role)
		}

	case forward.RuleTypeChain:
		// Handle chain rule based on agent's role
		switch rule.Role {
		case "entry":
			// Chain entry: connect to next hop
			t, err := a.getOrCreateTunnelByAddress(rule.NextHopAddress, rule.NextHopWsPort, rule.NextHopConnectionToken)
			if err != nil {
				return fmt.Errorf("create tunnel to next hop: %w", err)
			}

			ef := forwarder.NewEntryForwarder(rule, t)
			t.SetHandler(ef)
			if err := ef.Start(a.ctx); err != nil {
				return err
			}
			f = ef

		case "relay":
			// Chain relay: accept from previous hop, forward to next hop
			if err := a.ensureTunnelServer(); err != nil {
				return err
			}

			// Connect to next hop
			t, err := a.getOrCreateTunnelByAddress(rule.NextHopAddress, rule.NextHopWsPort, rule.NextHopConnectionToken)
			if err != nil {
				return fmt.Errorf("create tunnel to next hop: %w", err)
			}

			rf := forwarder.NewRelayForwarder(rule, t)
			a.tunnelServer.AddHandler(rule.ID, rf)
			if err := rf.Start(a.ctx); err != nil {
				return err
			}
			f = rf

		case "exit":
			// Chain exit: accept from previous hop, forward to target
			if err := a.ensureTunnelServer(); err != nil {
				return err
			}

			ef := forwarder.NewExitForwarder(rule)
			a.tunnelServer.AddHandler(rule.ID, ef)
			if err := ef.Start(a.ctx); err != nil {
				return err
			}
			f = ef

		default:
			return fmt.Errorf("unknown role %q for chain rule", rule.Role)
		}

	default:
		return fmt.Errorf("unknown rule type: %s", rule.RuleType)
	}

	a.forwardersMu.Lock()
	a.forwarders[rule.ID] = f
	a.forwardersMu.Unlock()

	logger.Info("forwarder started", "rule_id", rule.ID, "rule_type", rule.RuleType)
	return nil
}

func (a *Agent) getOrCreateTunnel(exitAgentID string) (*tunnel.Client, error) {
	a.tunnelsMu.Lock()
	defer a.tunnelsMu.Unlock()

	if t, exists := a.tunnels[exitAgentID]; exists {
		return t, nil
	}

	endpoint, err := a.client.GetExitEndpoint(a.ctx, exitAgentID)
	if err != nil {
		return nil, fmt.Errorf("get exit endpoint: %w", err)
	}

	wsURL := fmt.Sprintf("ws://%s:%d/tunnel", endpoint.Address, endpoint.WsPort)
	// Use ConnectionToken from API for agent-to-agent authentication
	t := tunnel.NewClient(wsURL, endpoint.ConnectionToken,
		tunnel.WithReconnectInterval(5*time.Second),
		tunnel.WithHeartbeatInterval(30*time.Second),
	)

	if err := t.Start(a.ctx); err != nil {
		return nil, fmt.Errorf("start tunnel: %w", err)
	}

	a.tunnels[exitAgentID] = t
	return t, nil
}

// getOrCreateTunnelByAddress creates a tunnel connection to a specific address.
// connectionToken is the JWT token for agent-to-agent authentication.
func (a *Agent) getOrCreateTunnelByAddress(address string, wsPort uint16, connectionToken string) (*tunnel.Client, error) {
	key := fmt.Sprintf("%s:%d", address, wsPort)

	a.tunnelsMu.Lock()
	defer a.tunnelsMu.Unlock()

	if t, exists := a.tunnels[key]; exists {
		return t, nil
	}

	wsURL := fmt.Sprintf("ws://%s:%d/tunnel", address, wsPort)
	t := tunnel.NewClient(wsURL, connectionToken,
		tunnel.WithReconnectInterval(5*time.Second),
		tunnel.WithHeartbeatInterval(30*time.Second),
	)

	if err := t.Start(a.ctx); err != nil {
		return nil, fmt.Errorf("start tunnel: %w", err)
	}

	a.tunnels[key] = t
	return t, nil
}

// ensureTunnelServer ensures the tunnel server is started.
// If WsListenPort is 0, a random available port will be used.
func (a *Agent) ensureTunnelServer() error {
	if a.tunnelServer != nil {
		return nil
	}

	a.tunnelServer = tunnel.NewServer(a.cfg.WsListenPort)
	if err := a.tunnelServer.Start(a.ctx); err != nil {
		return fmt.Errorf("start tunnel server: %w", err)
	}

	// Update config with actual port (important when port was 0)
	a.cfg.WsListenPort = a.tunnelServer.Port()

	return nil
}

func (a *Agent) trafficLoop() {
	defer a.wg.Done()

	ticker := time.NewTicker(a.cfg.TrafficInterval)
	defer ticker.Stop()

	for {
		select {
		case <-a.ctx.Done():
			return
		case <-ticker.C:
			a.reportTraffic()
		}
	}
}

func (a *Agent) statusLoop() {
	defer a.wg.Done()

	ticker := time.NewTicker(a.cfg.StatusInterval)
	defer ticker.Stop()

	for {
		select {
		case <-a.ctx.Done():
			return
		case <-ticker.C:
			a.reportStatus()
		}
	}
}

func (a *Agent) reportStatus() {
	st, err := a.collector.Collect(a.ctx)
	if err != nil {
		logger.Error("collect status failed", "error", err)
		return
	}

	// Set active rules and connections
	a.forwardersMu.RLock()
	activeRules := len(a.forwarders)
	a.forwardersMu.RUnlock()

	a.collector.SetActiveStats(st, activeRules, 0)

	// Set tunnel status
	a.tunnelsMu.RLock()
	tunnelStatus := make(map[string]forward.TunnelState)
	for exitAgentID, t := range a.tunnels {
		if t.IsConnected() {
			tunnelStatus[exitAgentID] = forward.TunnelStateConnected
		} else {
			tunnelStatus[exitAgentID] = forward.TunnelStateDisconnected
		}
	}
	a.tunnelsMu.RUnlock()

	a.collector.SetTunnelStatus(st, tunnelStatus)

	// Set WS listen port if configured (for exit/relay agents)
	if a.cfg.WsListenPort > 0 {
		st.WsListenPort = a.cfg.WsListenPort
	}

	if err := a.client.ReportStatus(a.ctx, st); err != nil {
		logger.Error("report status failed", "error", err)
		return
	}

	logger.Debug("status reported",
		"cpu", fmt.Sprintf("%.1f%%", st.CPUPercent),
		"mem", fmt.Sprintf("%.1f%%", st.MemoryPercent),
		"rules", st.ActiveRules)
}

func (a *Agent) reportTraffic() {
	a.forwardersMu.RLock()
	items := make([]forward.TrafficItem, 0, len(a.forwarders))
	for _, f := range a.forwarders {
		upload, download := f.Traffic().GetAndReset()
		if upload > 0 || download > 0 {
			items = append(items, forward.TrafficItem{
				RuleID:        f.RuleID(),
				UploadBytes:   upload,
				DownloadBytes: download,
			})
		}
	}
	a.forwardersMu.RUnlock()

	if len(items) == 0 {
		return
	}

	var totalUpload, totalDownload int64
	for _, item := range items {
		totalUpload += item.UploadBytes
		totalDownload += item.DownloadBytes
	}

	if err := a.client.ReportTraffic(a.ctx, items); err != nil {
		logger.Error("report traffic failed", "error", err)
		return
	}
	logger.Info("traffic reported",
		"rules", len(items),
		"upload_bytes", totalUpload,
		"download_bytes", totalDownload)
}

func (a *Agent) reportFinalTraffic() {
	a.forwardersMu.RLock()
	items := make([]forward.TrafficItem, 0, len(a.forwarders))
	for _, f := range a.forwarders {
		upload, download := f.Traffic().GetAndReset()
		if upload > 0 || download > 0 {
			items = append(items, forward.TrafficItem{
				RuleID:        f.RuleID(),
				UploadBytes:   upload,
				DownloadBytes: download,
			})
		}
	}
	a.forwardersMu.RUnlock()

	if len(items) == 0 {
		return
	}

	var totalUpload, totalDownload int64
	for _, item := range items {
		totalUpload += item.UploadBytes
		totalDownload += item.DownloadBytes
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := a.client.ReportTraffic(ctx, items); err != nil {
		logger.Error("report final traffic failed", "error", err)
		return
	}
	logger.Info("final traffic reported",
		"rules", len(items),
		"upload_bytes", totalUpload,
		"download_bytes", totalDownload)
}

func (a *Agent) stopAll() {
	a.forwardersMu.Lock()
	for _, f := range a.forwarders {
		f.Stop()
	}
	a.forwarders = make(map[string]forwarder.Forwarder)
	a.forwardersMu.Unlock()

	a.tunnelsMu.Lock()
	for _, t := range a.tunnels {
		t.Stop()
	}
	a.tunnels = make(map[string]*tunnel.Client)
	a.tunnelsMu.Unlock()

	if a.tunnelServer != nil {
		a.tunnelServer.Stop()
		a.tunnelServer = nil
	}
}

// hubLoop manages the Hub WebSocket connection with automatic reconnection.
func (a *Agent) hubLoop() {
	defer a.wg.Done()

	config := forward.DefaultReconnectConfig()
	config.OnConnected = func() {
		logger.Info("hub connected")
	}
	config.OnDisconnected = func(err error) {
		if err != nil && a.ctx.Err() == nil {
			logger.Warn("hub disconnected", "error", err)
		}
	}
	config.OnReconnecting = func(attempt uint64, delay time.Duration) {
		logger.Info("reconnecting to hub...", "attempt", attempt, "delay", delay)
	}

	// ProbeTaskHandler processes probe tasks and returns results
	probeHandler := func(task *forward.ProbeTask) *forward.ProbeTaskResult {
		return a.executeProbe(task)
	}

	if err := a.client.ForwardClient().RunHubLoopWithReconnect(a.ctx, probeHandler, config); err != nil {
		if a.ctx.Err() == nil {
			logger.Error("hub loop error", "error", err)
		}
	}
}

// executeProbe executes a probe task and returns the result.
func (a *Agent) executeProbe(task *forward.ProbeTask) *forward.ProbeTaskResult {
	result := &forward.ProbeTaskResult{
		TaskID: task.ID,
		Type:   task.Type,
		RuleID: task.RuleID,
	}

	timeout := time.Duration(task.Timeout) * time.Millisecond
	if timeout <= 0 {
		timeout = 5 * time.Second
	}

	start := time.Now()

	switch task.Type {
	case forward.ProbeTaskTypeTarget:
		result.Success, result.Error = a.probeTarget(task.Target, task.Port, task.Protocol, timeout)
	case forward.ProbeTaskTypeTunnel:
		result.Success, result.Error = a.probeTunnelByRule(task.RuleID)
	default:
		result.Success = false
		result.Error = fmt.Sprintf("unknown probe type: %s", task.Type)
	}

	result.LatencyMs = time.Since(start).Milliseconds()

	logger.Debug("probe executed",
		"task_id", task.ID,
		"type", task.Type,
		"success", result.Success,
		"latency_ms", result.LatencyMs)

	return result
}

// probeTarget probes a target address for connectivity.
func (a *Agent) probeTarget(target string, port uint16, protocol string, timeout time.Duration) (bool, string) {
	addr := net.JoinHostPort(target, fmt.Sprintf("%d", port))

	conn, err := net.DialTimeout(protocol, addr, timeout)
	if err != nil {
		return false, err.Error()
	}
	conn.Close()

	return true, ""
}

// probeTunnelByRule probes tunnel connectivity by rule ID.
func (a *Agent) probeTunnelByRule(ruleID string) (bool, string) {
	// Find the forwarder for this rule
	a.forwardersMu.RLock()
	f, exists := a.forwarders[ruleID]
	a.forwardersMu.RUnlock()

	if !exists {
		return false, "rule not found"
	}

	// Check if it's an entry forwarder with a tunnel
	if ef, ok := f.(*forwarder.EntryForwarder); ok {
		if ef.IsTunnelConnected() {
			return true, ""
		}
		return false, "tunnel disconnected"
	}

	// For non-entry rules, just check if forwarder exists
	return true, ""
}
