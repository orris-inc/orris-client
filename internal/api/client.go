package api

import (
	"context"
	"time"

	"github.com/orris-inc/orris/sdk/forward"
)

type Client struct {
	fc *forward.Client
}

func NewClient(baseURL, token string, timeout time.Duration) *Client {
	fc := forward.NewClient(baseURL, token, forward.WithTimeout(timeout))
	return &Client{fc: fc}
}

// ForwardClient returns the underlying forward.Client for Hub/Probe operations.
func (c *Client) ForwardClient() *forward.Client {
	return c.fc
}

func (c *Client) GetRules(ctx context.Context) ([]forward.Rule, error) {
	return c.fc.GetRules(ctx)
}

func (c *Client) ReportTraffic(ctx context.Context, items []forward.TrafficItem) error {
	if len(items) == 0 {
		return nil
	}
	_, err := c.fc.ReportTraffic(ctx, items)
	return err
}

func (c *Client) GetExitEndpoint(ctx context.Context, exitAgentID uint) (*forward.ExitEndpoint, error) {
	return c.fc.GetExitEndpoint(ctx, exitAgentID)
}

func (c *Client) ReportStatus(ctx context.Context, status *forward.AgentStatus) error {
	return c.fc.ReportStatus(ctx, status)
}
