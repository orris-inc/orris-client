package agent

import (
	"fmt"
	"time"

	"github.com/orris-inc/orris-client/internal/forward"
	"github.com/orris-inc/orris-client/internal/logger"
)

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
	// Update token info from full sync (ensures agent always has correct token)
	a.rulesMu.Lock()
	if data.ClientToken != "" {
		a.clientToken = data.ClientToken
	}
	if data.TokenSigningSecret != "" {
		a.signingSecret = data.TokenSigningSecret
	}
	a.rulesMu.Unlock()

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

// ruleSyncDataToRule converts RuleSyncData to forward.Rule.
func ruleSyncDataToRule(data *forward.RuleSyncData) *forward.Rule {
	return &forward.Rule{
		ID:                     data.ID,
		AgentID:                data.AgentID,
		RuleType:               forward.RuleType(data.RuleType),
		ListenPort:             data.ListenPort,
		TargetAddress:          data.TargetAddress,
		TargetPort:             data.TargetPort,
		BindIP:                 data.BindIP,
		Protocol:               data.Protocol,
		Role:                   data.Role,
		NextHopAgentID:         data.NextHopAgentID,
		NextHopAddress:         data.NextHopAddress,
		NextHopWsPort:          data.NextHopWsPort,
		NextHopPort:            data.NextHopPort,
		NextHopConnectionToken: data.NextHopConnectionToken,
		ChainAgentIDs:          data.ChainAgentIDs,
		ChainPosition:          data.ChainPosition,
		IsLastInChain:          data.IsLastInChain,
	}
}
