package forwarder

import (
	"context"
	"fmt"
	"sync"

	"github.com/orris-inc/orris/sdk/forward"

	"github.com/easayliu/orris-client/internal/logger"
)

type Forwarder interface {
	Start() error
	Stop()
	GetAndResetTraffic() (upload, download int64)
}

type ExitEndpointGetter interface {
	GetExitEndpoint(ctx context.Context, exitAgentID uint) (*forward.ExitEndpoint, error)
}

type ruleForwarders struct {
	rule  forward.Rule
	tcp   *TCPForwarder
	udp   *UDPForwarder
	entry *EntryForwarder
	exit  *ExitForwarder
}

type Manager struct {
	forwarders map[uint]*ruleForwarders
	mu         sync.RWMutex
	apiClient  ExitEndpointGetter
	token      string
}

func NewManager(apiClient ExitEndpointGetter, token string) *Manager {
	return &Manager{
		forwarders: make(map[uint]*ruleForwarders),
		apiClient:  apiClient,
		token:      token,
	}
}

func (m *Manager) SyncRules(rules []forward.Rule) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	newRuleIDs := make(map[uint]bool)
	for _, rule := range rules {
		newRuleIDs[rule.ID] = true
	}

	for id, rf := range m.forwarders {
		if !newRuleIDs[id] {
			m.stopForwarder(rf)
			delete(m.forwarders, id)
			logger.Info("stopped forwarder for removed rule", "rule_id", id, "name", rf.rule.Name)
		}
	}

	for _, rule := range rules {
		if existing, ok := m.forwarders[rule.ID]; ok {
			if m.ruleChanged(existing.rule, rule) {
				m.stopForwarder(existing)
				delete(m.forwarders, rule.ID)
				logger.Info("rule changed, restarting forwarder", "rule_id", rule.ID, "name", rule.Name)
			} else {
				continue
			}
		}

		rf, err := m.createForwarder(rule)
		if err != nil {
			logger.Error("failed to create forwarder", "rule_id", rule.ID, "name", rule.Name, "error", err)
			continue
		}
		m.forwarders[rule.ID] = rf
		logger.Info("started forwarder", "rule_id", rule.ID, "name", rule.Name, "port", rule.ListenPort)
	}

	return nil
}

func (m *Manager) ruleChanged(old, new forward.Rule) bool {
	return old.ListenPort != new.ListenPort ||
		old.TargetAddress != new.TargetAddress ||
		old.TargetPort != new.TargetPort ||
		old.Protocol != new.Protocol ||
		old.RuleType != new.RuleType ||
		old.ExitAgentID != new.ExitAgentID ||
		old.WsListenPort != new.WsListenPort
}

func (m *Manager) createForwarder(rule forward.Rule) (*ruleForwarders, error) {
	rf := &ruleForwarders{rule: rule}

	switch rule.RuleType {
	case forward.RuleTypeEntry:
		return m.createEntryForwarder(rf, rule)
	case forward.RuleTypeExit:
		return m.createExitForwarder(rf, rule)
	case forward.RuleTypeDirect, "":
		return m.createDirectForwarder(rf, rule)
	default:
		return nil, fmt.Errorf("unknown rule type: %s", rule.RuleType)
	}
}

func (m *Manager) createDirectForwarder(rf *ruleForwarders, rule forward.Rule) (*ruleForwarders, error) {
	switch rule.Protocol {
	case "tcp":
		rf.tcp = NewTCPForwarder(rule.ListenPort, rule.TargetAddress, rule.TargetPort)
		if err := rf.tcp.Start(); err != nil {
			return nil, fmt.Errorf("start tcp forwarder: %w", err)
		}
	case "udp":
		rf.udp = NewUDPForwarder(rule.ListenPort, rule.TargetAddress, rule.TargetPort)
		if err := rf.udp.Start(); err != nil {
			return nil, fmt.Errorf("start udp forwarder: %w", err)
		}
	case "both":
		rf.tcp = NewTCPForwarder(rule.ListenPort, rule.TargetAddress, rule.TargetPort)
		if err := rf.tcp.Start(); err != nil {
			return nil, fmt.Errorf("start tcp forwarder: %w", err)
		}
		rf.udp = NewUDPForwarder(rule.ListenPort, rule.TargetAddress, rule.TargetPort)
		if err := rf.udp.Start(); err != nil {
			rf.tcp.Stop()
			return nil, fmt.Errorf("start udp forwarder: %w", err)
		}
	default:
		return nil, fmt.Errorf("unknown protocol: %s", rule.Protocol)
	}
	return rf, nil
}

func (m *Manager) createEntryForwarder(rf *ruleForwarders, rule forward.Rule) (*ruleForwarders, error) {
	endpoint, err := m.apiClient.GetExitEndpoint(context.Background(), rule.ExitAgentID)
	if err != nil {
		return nil, fmt.Errorf("get exit endpoint: %w", err)
	}

	rf.entry = NewEntryForwarder(rule.ListenPort, endpoint.Address, endpoint.WsPort, rule.ID, m.token)
	if err := rf.entry.Start(); err != nil {
		return nil, fmt.Errorf("start entry forwarder: %w", err)
	}
	return rf, nil
}

func (m *Manager) createExitForwarder(rf *ruleForwarders, rule forward.Rule) (*ruleForwarders, error) {
	rf.exit = NewExitForwarder(rule.WsListenPort, rule.TargetAddress, rule.TargetPort, rule.ID, m.token)
	if err := rf.exit.Start(); err != nil {
		return nil, fmt.Errorf("start exit forwarder: %w", err)
	}
	return rf, nil
}

func (m *Manager) stopForwarder(rf *ruleForwarders) {
	if rf.tcp != nil {
		rf.tcp.Stop()
	}
	if rf.udp != nil {
		rf.udp.Stop()
	}
	if rf.entry != nil {
		rf.entry.Stop()
	}
	if rf.exit != nil {
		rf.exit.Stop()
	}
}

func (m *Manager) GetTrafficStats() []forward.TrafficItem {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var items []forward.TrafficItem
	for id, rf := range m.forwarders {
		var upload, download int64

		if rf.tcp != nil {
			u, d := rf.tcp.GetAndResetTraffic()
			upload += u
			download += d
		}
		if rf.udp != nil {
			u, d := rf.udp.GetAndResetTraffic()
			upload += u
			download += d
		}
		if rf.entry != nil {
			u, d := rf.entry.GetAndResetTraffic()
			upload += u
			download += d
		}
		if rf.exit != nil {
			u, d := rf.exit.GetAndResetTraffic()
			upload += u
			download += d
		}

		if upload > 0 || download > 0 {
			items = append(items, forward.TrafficItem{
				RuleID:        id,
				UploadBytes:   upload,
				DownloadBytes: download,
			})
		}
	}

	return items
}

func (m *Manager) Stop() {
	m.mu.Lock()
	defer m.mu.Unlock()

	for id, rf := range m.forwarders {
		m.stopForwarder(rf)
		logger.Info("stopped forwarder", "rule_id", id, "name", rf.rule.Name)
	}
	m.forwarders = make(map[uint]*ruleForwarders)
}
