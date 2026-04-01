package natsjs

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/nats-io/nats.go"
)

type Config struct {
	URL       string
	Domain    string
	CmdStream string
	EvtStream string
	WhkStream string
}

type Client struct {
	cfg Config
	nc  *nats.Conn
	js  nats.JetStreamContext
}

func New(cfg Config) (*Client, error) {
	if cfg.URL == "" {
		return nil, fmt.Errorf("nats url is required")
	}
	if cfg.CmdStream == "" {
		cfg.CmdStream = "AP_CMD"
	}
	if cfg.EvtStream == "" {
		cfg.EvtStream = "AP_EVT"
	}
	if cfg.WhkStream == "" {
		cfg.WhkStream = "AP_WHK"
	}

	nc, err := nats.Connect(cfg.URL)
	if err != nil {
		return nil, fmt.Errorf("connect nats: %w", err)
	}

	opts := make([]nats.JSOpt, 0, 1)
	if cfg.Domain != "" {
		opts = append(opts, nats.Domain(cfg.Domain))
	}
	js, err := nc.JetStream(opts...)
	if err != nil {
		nc.Close()
		return nil, fmt.Errorf("jetstream: %w", err)
	}

	return &Client{cfg: cfg, nc: nc, js: js}, nil
}

func (c *Client) Close() {
	if c.nc != nil {
		c.nc.Close()
	}
}

// JetStream 返回JetStream上下文
func (c *Client) JetStream() nats.JetStreamContext {
	return c.js
}

// Conn 返回NATS连接
func (c *Client) Conn() *nats.Conn {
	return c.nc
}

func (c *Client) EnsureStreams(_ context.Context) error {
	if err := c.ensureStream(c.cfg.CmdStream, []string{"cmd.>"}, nats.WorkQueuePolicy); err != nil {
		return err
	}
	if err := c.ensureStream(c.cfg.EvtStream, []string{"evt.>"}, nats.LimitsPolicy); err != nil {
		return err
	}
	if err := c.ensureStream(c.cfg.WhkStream, []string{"whk.>"}, nats.LimitsPolicy); err != nil {
		return err
	}
	return nil
}

func (c *Client) ensureStream(name string, subjects []string, retention nats.RetentionPolicy) error {
	config := &nats.StreamConfig{
		Name:      name,
		Subjects:  subjects,
		Storage:   nats.FileStorage,
		Retention: retention,
		MaxAge:    7 * 24 * time.Hour,
	}
	info, err := c.js.StreamInfo(name)
	if err == nil {
		current := info.Config
		if current.Retention != retention {
			current.Retention = retention
		}
		current.Subjects = subjects
		current.Storage = nats.FileStorage
		current.MaxAge = 7 * 24 * time.Hour
		if _, err := c.js.UpdateStream(&current); err != nil {
			return fmt.Errorf("update stream %s: %w", name, err)
		}
		return nil
	}
	if err != nats.ErrStreamNotFound {
		return fmt.Errorf("stream info %s: %w", name, err)
	}

	_, err = c.js.AddStream(config)
	if err != nil {
		return fmt.Errorf("add stream %s: %w", name, err)
	}
	return nil
}

func (c *Client) publishJSON(ctx context.Context, subject string, msgID string, payload any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal payload: %w", err)
	}
	msg := nats.NewMsg(subject)
	msg.Data = body
	if msgID != "" {
		msg.Header.Set(nats.MsgIdHdr, msgID)
	}
	if _, err := c.js.PublishMsg(msg, nats.Context(ctx)); err != nil {
		return fmt.Errorf("publish %s: %w", subject, err)
	}
	return nil
}

func (c *Client) PublishJSON(ctx context.Context, subject string, msgID string, payload any) error {
	return c.publishJSON(ctx, subject, msgID, payload)
}

func (c *Client) PublishCoreJSON(subject string, payload any) error {
	if c == nil || c.nc == nil {
		return fmt.Errorf("nats connection is not configured")
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal payload: %w", err)
	}
	if err := c.nc.Publish(subject, body); err != nil {
		return fmt.Errorf("publish core %s: %w", subject, err)
	}
	return nil
}
