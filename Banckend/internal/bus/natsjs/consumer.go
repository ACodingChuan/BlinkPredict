package natsjs

import (
	"fmt"
	"strings"

	"blinkpredict/banckend/internal/logging"

	"github.com/nats-io/nats.go"
)

func (c *Client) PullSubscribe(subject, durable string) (*nats.Subscription, error) {
	stream := c.streamForSubject(subject)
	sub, err := c.js.PullSubscribe(subject, durable, nats.BindStream(stream))
	if err != nil {
		return nil, fmt.Errorf("pull subscribe %s: %w", subject, err)
	}
	logging.Component("nats").Info().
		Str("kind", "nats_pull_subscribe").
		Str("subject", subject).
		Str("durable", durable).
		Str("stream", stream).
		Msg("nats pull subscribe created")
	return sub, nil
}

func (c *Client) ReplaySubscribe(subject string, startSeq uint64) (*nats.Subscription, error) {
	stream := c.streamForSubject(subject)
	opts := []nats.SubOpt{nats.BindStream(stream)}
	if startSeq > 0 {
		opts = append(opts, nats.StartSequence(startSeq))
	} else {
		opts = append(opts, nats.DeliverAll())
	}
	sub, err := c.js.PullSubscribe(subject, "", opts...)
	if err != nil {
		return nil, fmt.Errorf("replay subscribe %s: %w", subject, err)
	}
	logging.Component("nats").Info().
		Str("kind", "nats_replay_subscribe").
		Str("subject", subject).
		Str("stream", stream).
		Msg("nats replay subscribe created")
	return sub, nil
}

func (c *Client) StreamLastSeq(subject string) (uint64, error) {
	stream := c.streamForSubject(subject)
	info, err := c.js.StreamInfo(stream)
	if err != nil {
		return 0, fmt.Errorf("stream info %s: %w", stream, err)
	}
	if info == nil {
		return 0, nil
	}
	return info.State.LastSeq, nil
}

func (c *Client) streamForSubject(subject string) string {
	stream := c.cfg.CmdStream
	if strings.HasPrefix(subject, "evt.") {
		stream = c.cfg.EvtStream
	} else if strings.HasPrefix(subject, "whk.") {
		stream = c.cfg.WhkStream
	}
	return stream
}
