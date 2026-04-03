package chainconfirm

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"blinkpredict/banckend/internal/logging"

	solana "github.com/gagliardetto/solana-go"
	"github.com/gagliardetto/solana-go/rpc"
	rpcws "github.com/gagliardetto/solana-go/rpc/ws"
	"github.com/rs/zerolog"
)

type Result struct {
	Signature          string
	Slot               uint64
	ConfirmationStatus string
}

type Waiter struct {
	rpc   *rpc.Client
	wsURL string
	log   string
	mu    sync.Mutex
	ws    *rpcws.Client
}

func NewWaiter(rpcClient *rpc.Client, wsURL string, rpcURL string) *Waiter {
	return &Waiter{rpc: rpcClient, wsURL: deriveWSURL(wsURL, rpcURL), log: "chainconfirm"}
}

func (w *Waiter) WaitForConfirmed(ctx context.Context, signature solana.Signature) (Result, error) {
	if w == nil || w.rpc == nil {
		return Result{}, fmt.Errorf("chainconfirm rpc client is not configured")
	}
	logging.LogWS(w.log, ctx, zerolog.InfoLevel, "chain confirm wait start", map[string]any{
		"signature": signature.String(),
		"ws_url":    w.wsURL,
	})
	if status, ok, err := w.checkStatus(ctx, signature); err == nil && ok {
		logging.LogWS(w.log, ctx, zerolog.InfoLevel, "chain confirm http status hit", map[string]any{
			"signature": signature.String(),
			"slot":      status.Slot,
			"status":    status.ConfirmationStatus,
		})
		return status, nil
	}
	if strings.TrimSpace(w.wsURL) == "" {
		return w.waitWithHTTPFallback(ctx, signature, fmt.Errorf("solana ws url is not configured"))
	}

	res, err := w.waitViaWS(ctx, signature)
	if err == nil {
		logging.LogWS(w.log, ctx, zerolog.InfoLevel, "chain confirm websocket hit", map[string]any{
			"signature": signature.String(),
			"slot":      res.Slot,
			"status":    res.ConfirmationStatus,
		})
		return res, nil
	}
	logging.LogWS(w.log, ctx, zerolog.WarnLevel, "chain confirm websocket failed; retrying", map[string]any{
		"signature": signature.String(),
		"error":     err.Error(),
	})
	w.resetClient()
	res, retryErr := w.waitViaWS(ctx, signature)
	if retryErr == nil {
		logging.LogWS(w.log, ctx, zerolog.InfoLevel, "chain confirm websocket retry hit", map[string]any{
			"signature": signature.String(),
			"slot":      res.Slot,
			"status":    res.ConfirmationStatus,
		})
		return res, nil
	}
	logging.LogWS(w.log, ctx, zerolog.WarnLevel, "chain confirm websocket retry failed; fallback to http", map[string]any{
		"signature": signature.String(),
		"error":     retryErr.Error(),
	})
	return w.waitWithHTTPFallback(ctx, signature, retryErr)
}

func (w *Waiter) waitWithHTTPFallback(ctx context.Context, signature solana.Signature, triggerErr error) (Result, error) {
	status, ok, err := w.checkStatus(ctx, signature)
	if err == nil && ok {
		return status, nil
	}
	if err != nil {
		return Result{}, fmt.Errorf("confirm via ws failed (%v), fallback status check failed: %w", triggerErr, err)
	}
	return Result{}, fmt.Errorf("confirm via ws failed (%v), signature still unconfirmed", triggerErr)
}

func (w *Waiter) checkStatus(ctx context.Context, signature solana.Signature) (Result, bool, error) {
	resp, err := w.rpc.GetSignatureStatuses(ctx, true, signature)
	if err != nil {
		return Result{}, false, err
	}
	if resp == nil || len(resp.Value) == 0 || resp.Value[0] == nil {
		return Result{}, false, nil
	}
	status := resp.Value[0]
	confirm := strings.ToLower(strings.TrimSpace(string(status.ConfirmationStatus)))
	if confirm != string(rpc.CommitmentConfirmed) && confirm != string(rpc.CommitmentFinalized) {
		return Result{}, false, nil
	}
	if status.Err != nil {
		return Result{}, false, fmt.Errorf("signature %s failed on chain: %v", signature.String(), status.Err)
	}
	return Result{Signature: signature.String(), Slot: status.Slot, ConfirmationStatus: confirm}, true, nil
}

func deriveWSURL(explicit string, rpcURL string) string {
	if strings.TrimSpace(explicit) != "" {
		return strings.TrimSpace(explicit)
	}
	rpcURL = strings.TrimSpace(rpcURL)
	if rpcURL == "" {
		return ""
	}
	switch {
	case strings.HasPrefix(rpcURL, "https://"):
		return "wss://" + strings.TrimPrefix(rpcURL, "https://")
	case strings.HasPrefix(rpcURL, "http://"):
		return "ws://" + strings.TrimPrefix(rpcURL, "http://")
	default:
		return rpcURL
	}
}

func WithTimeout(parent context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if deadline, ok := parent.Deadline(); ok {
		remaining := time.Until(deadline)
		if remaining > 0 && remaining < timeout {
			return context.WithTimeout(parent, remaining)
		}
	}
	return context.WithTimeout(parent, timeout)
}

func (w *Waiter) waitViaWS(ctx context.Context, signature solana.Signature) (Result, error) {
	client, err := w.getClient(ctx)
	if err != nil {
		return Result{}, err
	}
	logging.LogWS(w.log, ctx, zerolog.InfoLevel, "signature subscribe start", map[string]any{
		"signature": signature.String(),
	})
	sub, err := client.SignatureSubscribe(signature, rpc.CommitmentConfirmed)
	if err != nil {
		return Result{}, err
	}
	defer sub.Unsubscribe()

	res, err := sub.Recv(ctx)
	if err != nil {
		return Result{}, err
	}
	if res == nil {
		return Result{}, fmt.Errorf("empty signature subscription result")
	}
	if res.Value.Err != nil {
		return Result{}, fmt.Errorf("signature %s confirmed with chain error: %v", signature.String(), res.Value.Err)
	}
	return Result{Signature: signature.String(), Slot: res.Context.Slot, ConfirmationStatus: string(rpc.CommitmentConfirmed)}, nil
}

func (w *Waiter) getClient(ctx context.Context) (*rpcws.Client, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.ws != nil {
		return w.ws, nil
	}
	logging.LogWS(w.log, ctx, zerolog.InfoLevel, "connect solana websocket", map[string]any{
		"ws_url": w.wsURL,
	})
	client, err := rpcws.Connect(ctx, w.wsURL)
	if err != nil {
		return nil, err
	}
	w.ws = client
	return w.ws, nil
}

func (w *Waiter) resetClient() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.ws != nil {
		w.ws.Close()
		w.ws = nil
	}
}
