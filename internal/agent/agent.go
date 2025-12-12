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
	tunnels   map[string]*tunnel.Client // ruleID -> tunnel

	tunnelServer  *tunnel.Server
	signingSecret string // also used as shared secret for key derivation
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

// syncLoop is a fallback mechanism for rule synchronization.
// Primary sync is done via WebSocket events from hub, but this loop ensures
// rules are eventually consistent even if WebSocket connection is unstable.
func (a *Agent) syncLoop() {
	defer a.wg.Done()

	// Use a longer interval since primary sync is via WebSocket events
	fallbackInterval := a.cfg.SyncInterval * 10 // 5 minutes by default
	if fallbackInterval < 5*time.Minute {
		fallbackInterval = 5 * time.Minute
	}

	ticker := time.NewTicker(fallbackInterval)
	defer ticker.Stop()

	for {
		select {
		case <-a.ctx.Done():
			return
		case <-ticker.C:
			if err := a.syncRules(); err != nil {
				logger.Error("fallback sync rules failed", "error", err)
			}
		}
	}
}

func (a *Agent) syncRules() error {
	logger.Info("requesting enabled rules")
	resp, err := a.client.GetRules(a.ctx)
	if err != nil {
		return err
	}

	rules := resp.Rules
	logger.Info("rules synced successfully", "count", len(rules))

	// Save signing secret and rules for handshake verification
	a.rulesMu.Lock()
	a.signingSecret = resp.TokenSigningSecret
	a.rules = rules
	a.rulesMu.Unlock()

	// Update tunnel server rules if exists
	if a.tunnelServer != nil {
		a.tunnelServer.UpdateRules(rules)
	}

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
			var t *tunnel.Client
			var err error

			// If NextHopAddress is already provided, use it directly
			// Otherwise, query endpoint via GetExitEndpoint if NextHopAgentID is set
			if rule.NextHopAddress != "" {
				t, err = a.getOrCreateTunnelByAddress(rule)
			} else if rule.NextHopAgentID != "" {
				t, err = a.getOrCreateTunnel(rule)
			} else {
				return fmt.Errorf("entry rule missing next hop info")
			}
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
			t, err := a.getOrCreateTunnelByAddress(rule)
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
			t, err := a.getOrCreateTunnelByAddress(rule)
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

	case forward.RuleTypeDirectChain:
		// Handle direct chain rule - uses direct TCP/UDP connections instead of WS tunnels
		// All roles (entry, relay, exit) use the same DirectChainForwarder
		// The difference is in NextHopAddress/NextHopPort vs TargetAddress/TargetPort
		dcf := forwarder.NewDirectChainForwarder(rule)
		if err := dcf.Start(a.ctx); err != nil {
			return err
		}
		f = dcf

	default:
		return fmt.Errorf("unknown rule type: %s", rule.RuleType)
	}

	a.forwardersMu.Lock()
	a.forwarders[rule.ID] = f
	a.forwardersMu.Unlock()

	logger.Info("forwarder started", "rule_id", rule.ID, "rule_type", rule.RuleType)
	return nil
}

func (a *Agent) getOrCreateTunnel(rule *forward.Rule) (*tunnel.Client, error) {
	a.tunnelsMu.Lock()
	defer a.tunnelsMu.Unlock()

	// Use ruleID as key since each rule needs its own tunnel connection for handshake
	if t, exists := a.tunnels[rule.ID]; exists {
		return t, nil
	}

	// Use NextHopAgentID to get endpoint (ExitAgentID is deprecated in RuleSyncData)
	agentID := rule.NextHopAgentID
	if agentID == "" {
		agentID = rule.ExitAgentID // fallback for legacy API response
	}
	if agentID == "" {
		return nil, fmt.Errorf("no next hop agent ID specified")
	}

	endpoint, err := a.client.GetExitEndpoint(a.ctx, agentID)
	if err != nil {
		return nil, fmt.Errorf("get exit endpoint: %w", err)
	}

	wsURL := fmt.Sprintf("ws://%s/tunnel", net.JoinHostPort(endpoint.Address, fmt.Sprintf("%d", endpoint.WsPort)))

	// Create endpoint refresher to handle exit agent restarts with port changes
	refresher := func() (string, string, error) {
		ep, err := a.client.GetExitEndpoint(a.ctx, agentID)
		if err != nil {
			return "", "", err
		}
		newURL := fmt.Sprintf("ws://%s/tunnel", net.JoinHostPort(ep.Address, fmt.Sprintf("%d", ep.WsPort)))
		return newURL, a.cfg.Token, nil
	}

	// Build client options with shared secret for forward-secure encryption
	opts := []tunnel.ClientOption{
		tunnel.WithHeartbeatInterval(30 * time.Second),
		tunnel.WithEndpointRefresher(refresher, 3), // refresh after 3 failed attempts
	}
	if a.signingSecret != "" {
		opts = append(opts, tunnel.WithSharedSecret(a.signingSecret))
	}

	// Use agent's own token for handshake authentication
	t := tunnel.NewClient(wsURL, a.cfg.Token, rule.ID, opts...)

	if err := t.Start(a.ctx); err != nil {
		return nil, fmt.Errorf("start tunnel: %w", err)
	}

	a.tunnels[rule.ID] = t
	return t, nil
}

// getOrCreateTunnelByAddress creates a tunnel connection to a specific address.
// Used for chain rules where the next hop address is explicitly provided.
func (a *Agent) getOrCreateTunnelByAddress(rule *forward.Rule) (*tunnel.Client, error) {
	a.tunnelsMu.Lock()
	defer a.tunnelsMu.Unlock()

	// Use ruleID as key since each rule needs its own tunnel connection for handshake
	if t, exists := a.tunnels[rule.ID]; exists {
		return t, nil
	}

	wsURL := fmt.Sprintf("ws://%s/tunnel", net.JoinHostPort(rule.NextHopAddress, fmt.Sprintf("%d", rule.NextHopWsPort)))

	// Use NextHopConnectionToken if available (short-term token for chain authentication),
	// otherwise fall back to agent's own token
	token := rule.NextHopConnectionToken
	if token == "" {
		token = a.cfg.Token
	}

	// Build client options with shared secret for forward-secure encryption
	opts := []tunnel.ClientOption{
		tunnel.WithHeartbeatInterval(30 * time.Second),
	}
	if a.signingSecret != "" {
		opts = append(opts, tunnel.WithSharedSecret(a.signingSecret))
	}

	t := tunnel.NewClient(wsURL, token, rule.ID, opts...)

	if err := t.Start(a.ctx); err != nil {
		return nil, fmt.Errorf("start tunnel: %w", err)
	}

	a.tunnels[rule.ID] = t
	return t, nil
}

// ensureTunnelServer ensures the tunnel server is started.
// If WsListenPort is 0, a random available port will be used.
func (a *Agent) ensureTunnelServer() error {
	if a.tunnelServer != nil {
		return nil
	}

	a.tunnelServer = tunnel.NewServer(a.cfg.WsListenPort, a.signingSecret, a.rules)
	if err := a.tunnelServer.Start(a.ctx); err != nil {
		return fmt.Errorf("start tunnel server: %w", err)
	}

	// Update config with actual port (important when port was 0)
	a.cfg.WsListenPort = a.tunnelServer.Port()

	// Immediately report status with new port so entry agents can reconnect
	go a.reportStatus()

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

	reconnectCfg := forward.DefaultReconnectConfig()
	reconnectCfg.OnConnected = func() {
		logger.Info("hub connected")
	}
	reconnectCfg.OnDisconnected = func(err error) {
		if err != nil && a.ctx.Err() == nil {
			logger.Warn("hub disconnected", "error", err)
		}
	}
	reconnectCfg.OnReconnecting = func(attempt uint64, delay time.Duration) {
		logger.Info("reconnecting to hub...", "attempt", attempt, "delay", delay)
	}

	a.runHubWithReconnect(reconnectCfg)
}

// runHubWithReconnect runs the hub connection loop with reconnection logic.
func (a *Agent) runHubWithReconnect(reconnectCfg *forward.ReconnectConfig) {
	backoff := time.Second
	maxBackoff := reconnectCfg.MaxInterval
	if maxBackoff == 0 {
		maxBackoff = 60 * time.Second
	}

	for {
		select {
		case <-a.ctx.Done():
			return
		default:
		}

		err := a.runHubOnce(reconnectCfg)
		if a.ctx.Err() != nil {
			return
		}

		if reconnectCfg.OnDisconnected != nil {
			reconnectCfg.OnDisconnected(err)
		}

		// Exponential backoff
		logger.Info("reconnecting to hub...", "delay", backoff)
		select {
		case <-a.ctx.Done():
			return
		case <-time.After(backoff):
		}

		backoff = time.Duration(float64(backoff) * reconnectCfg.Multiplier)
		if backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
}

// runHubOnce runs a single hub connection lifecycle.
func (a *Agent) runHubOnce(reconnectCfg *forward.ReconnectConfig) error {
	conn, err := a.client.ForwardClient().ConnectHub(a.ctx)
	if err != nil {
		return fmt.Errorf("connect hub: %w", err)
	}
	defer conn.Close()

	if reconnectCfg.OnConnected != nil {
		reconnectCfg.OnConnected()
	}

	// Start connection read/write pumps in background
	connErrCh := make(chan error, 1)
	go func() {
		connErrCh <- conn.Run(a.ctx)
	}()

	// Process events from hub
	for {
		select {
		case <-a.ctx.Done():
			return a.ctx.Err()

		case err := <-connErrCh:
			return err

		case event, ok := <-conn.Events:
			if !ok {
				return fmt.Errorf("events channel closed")
			}
			a.handleHubEvent(conn, event)
		}
	}
}

// handleHubEvent processes events from hub connection.
func (a *Agent) handleHubEvent(conn *forward.HubConn, event *forward.HubEvent) {
	switch event.Type {
	case forward.HubEventConfigSync:
		if event.ConfigSync != nil {
			a.handleConfigSync(conn, event.ConfigSync)
		}

	case forward.HubEventProbeTask:
		if event.ProbeTask != nil {
			go func() {
				result := a.executeProbe(event.ProbeTask)
				if result != nil {
					conn.SendProbeResult(result)
				}
			}()
		}
	}
}

// handleConfigSync processes configuration sync events from hub.
func (a *Agent) handleConfigSync(conn *forward.HubConn, data *forward.ConfigSyncData) {
	logger.Info("received config sync",
		"version", data.Version,
		"full_sync", data.FullSync,
		"added", len(data.Added),
		"updated", len(data.Updated),
		"removed", len(data.Removed))

	// Skip if we already have this version or newer
	if data.Version <= a.configVersion && !data.FullSync {
		logger.Debug("skipping config sync, already at version", "current", a.configVersion, "received", data.Version)
		conn.SendConfigAck(&forward.ConfigAckData{
			Version: data.Version,
			Success: true,
		})
		return
	}

	var syncErr error

	// Handle full sync - stop all and restart
	if data.FullSync {
		syncErr = a.handleFullSync(data)
	} else {
		syncErr = a.handleIncrementalSync(data)
	}

	// Send acknowledgment
	ack := &forward.ConfigAckData{
		Version: data.Version,
		Success: syncErr == nil,
	}
	if syncErr != nil {
		ack.Error = syncErr.Error()
		logger.Error("config sync failed", "error", syncErr)
	} else {
		a.configVersion = data.Version
		logger.Info("config sync completed", "version", data.Version)
	}

	conn.SendConfigAck(ack)
}

// handleFullSync handles full configuration sync.
func (a *Agent) handleFullSync(data *forward.ConfigSyncData) error {
	// Build rule map from added rules
	newRules := make(map[string]*forward.Rule)
	for i := range data.Added {
		rule := ruleSyncDataToRule(&data.Added[i])
		newRules[rule.ID] = rule
	}

	// Stop forwarders not in new rules
	a.forwardersMu.Lock()
	for ruleID, f := range a.forwarders {
		if _, exists := newRules[ruleID]; !exists {
			logger.Info("stopping forwarder for removed rule", "rule_id", ruleID)
			f.Stop()
			delete(a.forwarders, ruleID)
		}
	}
	a.forwardersMu.Unlock()

	// Update rules list for tunnel server
	a.rulesMu.Lock()
	a.rules = make([]forward.Rule, 0, len(newRules))
	for _, rule := range newRules {
		a.rules = append(a.rules, *rule)
	}
	a.rulesMu.Unlock()

	if a.tunnelServer != nil {
		a.rulesMu.RLock()
		a.tunnelServer.UpdateRules(a.rules)
		a.rulesMu.RUnlock()
	}

	// Start forwarders for new rules
	for _, rule := range newRules {
		a.forwardersMu.RLock()
		_, exists := a.forwarders[rule.ID]
		a.forwardersMu.RUnlock()

		if !exists {
			if err := a.startForwarder(rule); err != nil {
				logger.Error("start forwarder failed", "rule_id", rule.ID, "error", err)
			}
		}
	}

	return nil
}

// handleIncrementalSync handles incremental configuration sync.
func (a *Agent) handleIncrementalSync(data *forward.ConfigSyncData) error {
	// Handle removed rules
	for _, ruleID := range data.Removed {
		a.stopForwarder(ruleID)
	}

	// Handle updated rules (stop then start)
	for i := range data.Updated {
		rule := ruleSyncDataToRule(&data.Updated[i])
		a.stopForwarder(rule.ID)
		if err := a.startForwarder(rule); err != nil {
			logger.Error("restart forwarder failed", "rule_id", rule.ID, "error", err)
		}
	}

	// Handle added rules
	for i := range data.Added {
		rule := ruleSyncDataToRule(&data.Added[i])
		if err := a.startForwarder(rule); err != nil {
			logger.Error("start forwarder failed", "rule_id", rule.ID, "error", err)
		}
	}

	// Update rules list
	a.updateRulesList(data)

	return nil
}

// stopForwarder stops and removes a forwarder by rule ID.
func (a *Agent) stopForwarder(ruleID string) {
	a.forwardersMu.Lock()
	if f, exists := a.forwarders[ruleID]; exists {
		logger.Info("stopping forwarder", "rule_id", ruleID)
		f.Stop()
		delete(a.forwarders, ruleID)
	}
	a.forwardersMu.Unlock()

	// Also stop tunnel if exists
	a.tunnelsMu.Lock()
	if t, exists := a.tunnels[ruleID]; exists {
		t.Stop()
		delete(a.tunnels, ruleID)
	}
	a.tunnelsMu.Unlock()
}

// updateRulesList updates the internal rules list based on sync data.
func (a *Agent) updateRulesList(data *forward.ConfigSyncData) {
	a.rulesMu.Lock()
	defer a.rulesMu.Unlock()

	// Build map from current rules
	ruleMap := make(map[string]*forward.Rule)
	for i := range a.rules {
		ruleMap[a.rules[i].ID] = &a.rules[i]
	}

	// Remove
	for _, ruleID := range data.Removed {
		delete(ruleMap, ruleID)
	}

	// Add/Update
	for i := range data.Added {
		rule := ruleSyncDataToRule(&data.Added[i])
		ruleMap[rule.ID] = rule
	}
	for i := range data.Updated {
		rule := ruleSyncDataToRule(&data.Updated[i])
		ruleMap[rule.ID] = rule
	}

	// Rebuild rules slice
	a.rules = make([]forward.Rule, 0, len(ruleMap))
	for _, rule := range ruleMap {
		a.rules = append(a.rules, *rule)
	}

	// Update tunnel server
	if a.tunnelServer != nil {
		a.tunnelServer.UpdateRules(a.rules)
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

// ruleSyncDataToRule converts RuleSyncData to forward.Rule.
func ruleSyncDataToRule(data *forward.RuleSyncData) *forward.Rule {
	return &forward.Rule{
		ID:             data.ID,
		AgentID:        data.AgentID,
		RuleType:       forward.RuleType(data.RuleType),
		ListenPort:     data.ListenPort,
		TargetAddress:  data.TargetAddress,
		TargetPort:     data.TargetPort,
		BindIP:         data.BindIP,
		Protocol:       data.Protocol,
		Role:           data.Role,
		NextHopAgentID: data.NextHopAgentID,
		NextHopAddress: data.NextHopAddress,
		NextHopWsPort:  data.NextHopWsPort,
		NextHopPort:    data.NextHopPort,
		ChainAgentIDs:  data.ChainAgentIDs,
		ChainPosition:  data.ChainPosition,
		IsLastInChain:  data.IsLastInChain,
	}
}
