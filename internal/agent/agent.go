package agent

import (
	"context"
	"encoding/json"
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
	forwarders   map[uint]forwarder.Forwarder

	tunnelsMu sync.RWMutex
	tunnels   map[uint]*tunnel.Client // exitAgentID -> tunnel

	tunnelServer *tunnel.Server

	// Hub connection for WebSocket communication
	hubMu   sync.RWMutex
	hubConn *forward.HubConn

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
		forwarders: make(map[uint]forwarder.Forwarder),
		tunnels:    make(map[uint]*tunnel.Client),
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
	a.closeHub()
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

	ruleMap := make(map[uint]*forward.Rule)
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

	case forward.RuleTypeExit:
		if a.tunnelServer == nil {
			a.tunnelServer = tunnel.NewServer(rule.WsListenPort, a.cfg.Token)
			if err := a.tunnelServer.Start(a.ctx); err != nil {
				return fmt.Errorf("start tunnel server: %w", err)
			}
		}

		ef := forwarder.NewExitForwarder(rule)
		a.tunnelServer.AddHandler(rule.ID, ef)
		if err := ef.Start(a.ctx); err != nil {
			return err
		}
		f = ef

	default:
		return fmt.Errorf("unknown rule type: %s", rule.RuleType)
	}

	a.forwardersMu.Lock()
	a.forwarders[rule.ID] = f
	a.forwardersMu.Unlock()

	logger.Info("forwarder started", "rule_id", rule.ID, "rule_type", rule.RuleType)
	return nil
}

func (a *Agent) getOrCreateTunnel(exitAgentID uint) (*tunnel.Client, error) {
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
	t := tunnel.NewClient(wsURL, a.cfg.Token,
		tunnel.WithReconnectInterval(5*time.Second),
		tunnel.WithHeartbeatInterval(30*time.Second),
	)

	if err := t.Start(a.ctx); err != nil {
		return nil, fmt.Errorf("start tunnel: %w", err)
	}

	a.tunnels[exitAgentID] = t
	return t, nil
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
	tunnelStatus := make(map[uint]forward.TunnelState)
	for exitAgentID, t := range a.tunnels {
		if t.IsConnected() {
			tunnelStatus[exitAgentID] = forward.TunnelStateConnected
		} else {
			tunnelStatus[exitAgentID] = forward.TunnelStateDisconnected
		}
	}
	a.tunnelsMu.RUnlock()

	a.collector.SetTunnelStatus(st, tunnelStatus)

	// Try to send via Hub WebSocket first, fallback to HTTP
	if conn := a.getHubConn(); conn != nil {
		if err := conn.SendStatus(st); err != nil {
			logger.Debug("hub send status failed, falling back to HTTP", "error", err)
			a.reportStatusHTTP(st)
		} else {
			logger.Debug("status reported via hub",
				"cpu", fmt.Sprintf("%.1f%%", st.CPUPercent),
				"mem", fmt.Sprintf("%.1f%%", st.MemoryPercent),
				"rules", st.ActiveRules)
		}
	} else {
		a.reportStatusHTTP(st)
	}
}

func (a *Agent) reportStatusHTTP(st *forward.AgentStatus) {
	if err := a.client.ReportStatus(a.ctx, st); err != nil {
		logger.Error("report status failed", "error", err)
		return
	}

	logger.Debug("status reported via http",
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
	a.forwarders = make(map[uint]forwarder.Forwarder)
	a.forwardersMu.Unlock()

	a.tunnelsMu.Lock()
	for _, t := range a.tunnels {
		t.Stop()
	}
	a.tunnels = make(map[uint]*tunnel.Client)
	a.tunnelsMu.Unlock()

	if a.tunnelServer != nil {
		a.tunnelServer.Stop()
		a.tunnelServer = nil
	}
}

// hubLoop manages the Hub WebSocket connection with automatic reconnection.
func (a *Agent) hubLoop() {
	defer a.wg.Done()

	reconnectInterval := 5 * time.Second

	for {
		select {
		case <-a.ctx.Done():
			return
		default:
		}

		if err := a.runHub(); err != nil {
			if a.ctx.Err() != nil {
				return
			}
			logger.Error("hub connection error", "error", err)
		}

		select {
		case <-a.ctx.Done():
			return
		case <-time.After(reconnectInterval):
			logger.Info("reconnecting to hub...")
		}
	}
}

// runHub establishes and runs the Hub connection.
func (a *Agent) runHub() error {
	conn, err := a.client.ForwardClient().ConnectHub(a.ctx)
	if err != nil {
		return fmt.Errorf("connect hub: %w", err)
	}

	a.hubMu.Lock()
	a.hubConn = conn
	a.hubMu.Unlock()

	logger.Info("hub connected")

	// Set up message handler for probe tasks
	conn.SetMessageHandler(func(msg *forward.HubMessage) {
		switch msg.Type {
		case forward.MsgTypeProbeTask:
			go a.handleProbeTask(msg)
		case forward.MsgTypeCommand:
			go a.handleCommand(msg)
		}
	})

	// Run the connection (blocks until error or context done)
	err = conn.Run(a.ctx)

	a.hubMu.Lock()
	a.hubConn = nil
	a.hubMu.Unlock()

	if err != nil && a.ctx.Err() == nil {
		logger.Warn("hub disconnected", "error", err)
	}

	return err
}

// closeHub closes the current Hub connection.
func (a *Agent) closeHub() {
	a.hubMu.Lock()
	defer a.hubMu.Unlock()

	if a.hubConn != nil {
		a.hubConn.Close()
		a.hubConn = nil
	}
}

// getHubConn returns the current Hub connection if available.
func (a *Agent) getHubConn() *forward.HubConn {
	a.hubMu.RLock()
	defer a.hubMu.RUnlock()
	return a.hubConn
}

// handleProbeTask handles probe task messages from the Hub.
func (a *Agent) handleProbeTask(msg *forward.HubMessage) {
	task := parseProbeTask(msg.Data)
	if task == nil {
		logger.Error("failed to parse probe task")
		return
	}

	result := a.executeProbe(task)
	if result == nil {
		return
	}

	conn := a.getHubConn()
	if conn != nil {
		if err := conn.SendProbeResult(result); err != nil {
			logger.Error("failed to send probe result", "error", err)
		}
	}
}

// handleCommand handles command messages from the Hub.
func (a *Agent) handleCommand(msg *forward.HubMessage) {
	logger.Debug("received command", "data", msg.Data)
	// TODO: Implement command handling (e.g., force sync, restart forwarder)
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
func (a *Agent) probeTunnelByRule(ruleID uint) (bool, string) {
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

// parseProbeTask parses probe task from message data.
func parseProbeTask(data any) *forward.ProbeTask {
	if data == nil {
		return nil
	}

	// Try direct type assertion first
	if task, ok := data.(*forward.ProbeTask); ok {
		return task
	}

	// Use JSON marshal/unmarshal for map conversion
	dataBytes, err := json.Marshal(data)
	if err != nil {
		return nil
	}

	var task forward.ProbeTask
	if err := json.Unmarshal(dataBytes, &task); err != nil {
		return nil
	}

	return &task
}
