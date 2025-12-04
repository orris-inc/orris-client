package agent

import (
	"context"
	"fmt"
	"time"

	"github.com/orris-inc/orris-client/internal/api"
	"github.com/orris-inc/orris-client/internal/config"
	"github.com/orris-inc/orris-client/internal/forwarder"
	"github.com/orris-inc/orris-client/internal/logger"
)

type Agent struct {
	cfg      *config.Config
	client   *api.Client
	manager  *forwarder.Manager
	ctx      context.Context
	cancelFn context.CancelFunc
	doneChan chan struct{}
}

func New(cfg *config.Config) *Agent {
	client := api.NewClient(cfg.ServerURL, cfg.Token, cfg.HTTPTimeout)
	manager := forwarder.NewManager(client, cfg.Token)

	return &Agent{
		cfg:      cfg,
		client:   client,
		manager:  manager,
		doneChan: make(chan struct{}),
	}
}

func (a *Agent) Start(ctx context.Context) error {
	a.ctx, a.cancelFn = context.WithCancel(ctx)

	if err := a.syncRules(); err != nil {
		return fmt.Errorf("initial sync failed: %w", err)
	}

	go a.syncLoop()
	go a.trafficLoop()

	return nil
}

func (a *Agent) Stop() {
	if a.cancelFn != nil {
		a.cancelFn()
	}

	a.reportFinalTraffic()
	a.manager.Stop()

	close(a.doneChan)
}

func (a *Agent) Wait() {
	<-a.doneChan
}

func (a *Agent) syncLoop() {
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
	return a.manager.SyncRules(rules)
}

func (a *Agent) trafficLoop() {
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

func (a *Agent) reportTraffic() {
	items := a.manager.GetTrafficStats()
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
	items := a.manager.GetTrafficStats()
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
