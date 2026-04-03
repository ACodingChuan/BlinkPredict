package logging

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/rs/zerolog"
)

type pgxTraceContextKey string

const pgxQueryStartKey pgxTraceContextKey = "pgx_query_start"

func NewPGXTracer(component string) *PGXTracer {
	return &PGXTracer{logger: Component(component)}
}

type PGXTracer struct {
	logger *zerolog.Logger
}

func (t *PGXTracer) TraceConnectStart(ctx context.Context, data pgx.TraceConnectStartData) context.Context {
	if t == nil || t.logger == nil {
		return ctx
	}
	EventWithContext(ctx, t.logger.Info()).
		Str("kind", "sql_connect_start").
		Str("host", data.ConnConfig.Host).
		Uint16("port", data.ConnConfig.Port).
		Str("database", data.ConnConfig.Database).
		Str("user", data.ConnConfig.User).
		Msg("postgres connect start")
	return ctx
}

func (t *PGXTracer) TraceConnectEnd(ctx context.Context, data pgx.TraceConnectEndData) {
	if t == nil || t.logger == nil {
		return
	}
	event := EventWithContext(ctx, t.logger.Info()).Str("kind", "sql_connect_end")
	if data.Err != nil {
		event = EventWithContext(ctx, t.logger.Error()).Str("kind", "sql_connect_end").Err(data.Err)
	}
	event.Msg("postgres connect end")
}

func (t *PGXTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if t == nil || t.logger == nil {
		return ctx
	}
	EventWithContext(ctx, t.logger.Info()).
		Str("kind", "sql_query_start").
		Str("sql", data.SQL).
		Any("args", FormatArgs(data.Args)).
		Msg("postgres query start")
	return context.WithValue(ctx, pgxQueryStartKey, time.Now())
}

func (t *PGXTracer) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryEndData) {
	if t == nil || t.logger == nil {
		return
	}
	event := EventWithContext(ctx, t.logger.Info()).
		Str("kind", "sql_query_end").
		Str("command_tag", data.CommandTag.String())
	if startedAt, ok := ctx.Value(pgxQueryStartKey).(time.Time); ok {
		event = event.Dur("latency", time.Since(startedAt))
	}
	if data.Err != nil {
		event = EventWithContext(ctx, t.logger.Error()).
			Str("kind", "sql_query_end").
			Str("command_tag", data.CommandTag.String()).
			Err(data.Err)
		if startedAt, ok := ctx.Value(pgxQueryStartKey).(time.Time); ok {
			event = event.Dur("latency", time.Since(startedAt))
		}
	}
	event.Msg("postgres query end")
}

func (t *PGXTracer) TraceBatchStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceBatchStartData) context.Context {
	if t == nil || t.logger == nil {
		return ctx
	}
	EventWithContext(ctx, t.logger.Info()).
		Str("kind", "sql_batch_start").
		Msg("postgres batch start")
	return context.WithValue(ctx, pgxQueryStartKey, time.Now())
}

func (t *PGXTracer) TraceBatchQuery(ctx context.Context, _ *pgx.Conn, data pgx.TraceBatchQueryData) {
	if t == nil || t.logger == nil {
		return
	}
	event := EventWithContext(ctx, t.logger.Info()).
		Str("kind", "sql_batch_query").
		Str("sql", data.SQL).
		Any("args", FormatArgs(data.Args)).
		Str("command_tag", data.CommandTag.String())
	if data.Err != nil {
		event = EventWithContext(ctx, t.logger.Error()).
			Str("kind", "sql_batch_query").
			Str("sql", data.SQL).
			Any("args", FormatArgs(data.Args)).
			Str("command_tag", data.CommandTag.String()).
			Err(data.Err)
	}
	event.Msg("postgres batch query")
}

func (t *PGXTracer) TraceBatchEnd(ctx context.Context, _ *pgx.Conn, data pgx.TraceBatchEndData) {
	if t == nil || t.logger == nil {
		return
	}
	event := EventWithContext(ctx, t.logger.Info()).Str("kind", "sql_batch_end")
	if startedAt, ok := ctx.Value(pgxQueryStartKey).(time.Time); ok {
		event = event.Dur("latency", time.Since(startedAt))
	}
	if data.Err != nil {
		event = EventWithContext(ctx, t.logger.Error()).Str("kind", "sql_batch_end").Err(data.Err)
		if startedAt, ok := ctx.Value(pgxQueryStartKey).(time.Time); ok {
			event = event.Dur("latency", time.Since(startedAt))
		}
	}
	event.Msg("postgres batch end")
}
