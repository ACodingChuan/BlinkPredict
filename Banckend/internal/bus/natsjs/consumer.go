package natsjs

import (
	"fmt"
	"strings"

	"github.com/nats-io/nats.go"
)

func (c *Client) PullSubscribe(subject, durable string) (*nats.Subscription, error) {
	stream := c.cfg.CmdStream
	if strings.HasPrefix(subject, "evt.") {
		stream = c.cfg.EvtStream
	} else if strings.HasPrefix(subject, "whk.") {
		stream = c.cfg.WhkStream
	}
	sub, err := c.js.PullSubscribe(subject, durable, nats.BindStream(stream))
	if err != nil {
		return nil, fmt.Errorf("pull subscribe %s: %w", subject, err)
	}
	return sub, nil
}
