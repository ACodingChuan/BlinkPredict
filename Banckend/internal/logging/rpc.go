package logging

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"time"

	"github.com/gagliardetto/solana-go/rpc"
	"github.com/gagliardetto/solana-go/rpc/jsonrpc"
	"github.com/klauspost/compress/gzhttp"
	"github.com/rs/zerolog"
)

const defaultRPCBodyLimit = 256 * 1024

type LoggingRoundTripper struct {
	component string
	logger    *zerolog.Logger
	base      http.RoundTripper
	bodyLimit int
}

func NewSolanaRPCClient(component string, endpoint string) *rpc.Client {
	transport := &http.Transport{
		IdleConnTimeout:     5 * time.Minute,
		MaxConnsPerHost:     9,
		MaxIdleConnsPerHost: 9,
		Proxy:               http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   5 * time.Minute,
			KeepAlive: 180 * time.Second,
			DualStack: true,
		}).DialContext,
		ForceAttemptHTTP2:   true,
		TLSHandshakeTimeout: 10 * time.Second,
	}
	httpClient := &http.Client{
		Timeout: 5 * time.Minute,
		Transport: &LoggingRoundTripper{
			component: component,
			logger:    Component(component),
			base:      gzhttp.Transport(transport),
			bodyLimit: defaultRPCBodyLimit,
		},
	}
	rpcClient := jsonrpc.NewClientWithOpts(endpoint, &jsonrpc.RPCClientOpts{HTTPClient: httpClient})
	return rpc.NewWithCustomRPCClient(rpcClient)
}

func (t *LoggingRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	if t == nil || t.base == nil {
		return nil, http.ErrServerClosed
	}
	start := time.Now()
	requestBody, requestTruncated, requestErr := cloneBody(req.Body, t.bodyLimit)
	if requestErr == nil {
		req.Body = io.NopCloser(bytes.NewReader(requestBody))
	}

	resp, err := t.base.RoundTrip(req)
	if err != nil {
		EventWithContext(req.Context(), t.logger.Error()).
			Str("kind", "rpc_http_roundtrip").
			Str("component", t.component).
			Str("method", req.Method).
			Str("url", req.URL.String()).
			Any("rpc_request", extractRPCPayload(requestBody)).
			Str("request_body", prettyJSON(requestBody)).
			Bool("request_body_truncated", requestTruncated).
			Dur("latency", time.Since(start)).
			Err(err).
			Msg("solana rpc roundtrip failed")
		return nil, err
	}

	responseBody, responseTruncated, responseErr := cloneBody(resp.Body, t.bodyLimit)
	if responseErr == nil {
		resp.Body = io.NopCloser(bytes.NewReader(responseBody))
	}

	event := EventWithContext(req.Context(), t.logger.Info()).
		Str("kind", "rpc_http_roundtrip").
		Str("component", t.component).
		Str("method", req.Method).
		Str("url", req.URL.String()).
		Int("status", resp.StatusCode).
		Any("rpc_request", extractRPCPayload(requestBody)).
		Str("request_body", prettyJSON(requestBody)).
		Bool("request_body_truncated", requestTruncated).
		Str("response_body", prettyJSON(responseBody)).
		Bool("response_body_truncated", responseTruncated).
		Dur("latency", time.Since(start))
	if responseErr != nil {
		event = EventWithContext(req.Context(), t.logger.Error()).
			Str("kind", "rpc_http_roundtrip").
			Str("component", t.component).
			Str("method", req.Method).
			Str("url", req.URL.String()).
			Int("status", resp.StatusCode).
			Any("rpc_request", extractRPCPayload(requestBody)).
			Str("request_body", prettyJSON(requestBody)).
			Bool("request_body_truncated", requestTruncated).
			Dur("latency", time.Since(start)).
			Err(responseErr)
	}
	event.Msg("solana rpc roundtrip completed")
	return resp, nil
}

func cloneBody(body io.ReadCloser, limit int) ([]byte, bool, error) {
	if body == nil {
		return nil, false, nil
	}
	defer body.Close()
	buf, err := io.ReadAll(io.LimitReader(body, int64(limit+1)))
	if err != nil {
		return nil, false, err
	}
	truncated := len(buf) > limit
	if truncated {
		buf = buf[:limit]
	}
	return buf, truncated, nil
}

func extractRPCPayload(body []byte) map[string]any {
	body = bytes.TrimSpace(body)
	if len(body) == 0 {
		return nil
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return map[string]any{"raw": string(body)}
	}
	result := map[string]any{}
	for _, key := range []string{"method", "params", "id", "jsonrpc"} {
		if value, ok := payload[key]; ok {
			result[key] = value
		}
	}
	if len(result) == 0 {
		return map[string]any{"raw": string(body)}
	}
	return result
}

func LogWS(component string, ctx context.Context, level zerolog.Level, msg string, fields map[string]any) {
	logger := Component(component)
	event := EventWithContext(ctx, logger.WithLevel(level)).Str("kind", "solana_ws")
	for key, value := range fields {
		event = event.Interface(key, value)
	}
	event.Msg(msg)
}
