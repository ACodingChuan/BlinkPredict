package logging

import (
	"context"
	"net"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/rs/zerolog"
)

func NewRedisHook(component string) redis.Hook {
	return &RedisHook{logger: Component(component)}
}

type RedisHook struct {
	logger *zerolog.Logger
}

func (h *RedisHook) DialHook(next redis.DialHook) redis.DialHook {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		start := time.Now()
		conn, err := next(ctx, network, addr)
		event := EventWithContext(ctx, h.logger.Info()).
			Str("kind", "redis_dial").
			Str("network", network).
			Str("addr", addr).
			Dur("latency", time.Since(start))
		if err != nil {
			event = EventWithContext(ctx, h.logger.Error()).
				Str("kind", "redis_dial").
				Str("network", network).
				Str("addr", addr).
				Dur("latency", time.Since(start)).
				Err(err)
		}
		event.Msg("redis dial")
		return conn, err
	}
}

func (h *RedisHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		start := time.Now()
		err := next(ctx, cmd)
		event := EventWithContext(ctx, h.logger.Info()).
			Str("kind", "redis_command").
			Str("command", cmd.Name()).
			Any("args", FormatArgs(cmd.Args())).
			Str("result", cmd.String()).
			Dur("latency", time.Since(start))
		if err != nil {
			event = EventWithContext(ctx, h.logger.Error()).
				Str("kind", "redis_command").
				Str("command", cmd.Name()).
				Any("args", FormatArgs(cmd.Args())).
				Str("result", cmd.String()).
				Dur("latency", time.Since(start)).
				Err(err)
		}
		event.Msg("redis command executed")
		return err
	}
}

func (h *RedisHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []redis.Cmder) error {
		start := time.Now()
		err := next(ctx, cmds)
		entries := make([]map[string]any, 0, len(cmds))
		for _, cmd := range cmds {
			entries = append(entries, map[string]any{
				"command": cmd.Name(),
				"args":    FormatArgs(cmd.Args()),
				"result":  cmd.String(),
			})
		}
		event := EventWithContext(ctx, h.logger.Info()).
			Str("kind", "redis_pipeline").
			Any("commands", entries).
			Dur("latency", time.Since(start))
		if err != nil {
			event = EventWithContext(ctx, h.logger.Error()).
				Str("kind", "redis_pipeline").
				Any("commands", entries).
				Dur("latency", time.Since(start)).
				Err(err)
		}
		event.Msg("redis pipeline executed")
		return err
	}
}
