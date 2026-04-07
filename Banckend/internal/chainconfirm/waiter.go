package chainconfirm

import (
	"context"
	"fmt"
	"strings"
	"time"

	"blinkpredict/banckend/internal/logging"

	solana "github.com/gagliardetto/solana-go"
	"github.com/gagliardetto/solana-go/rpc"
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
	rt    WSRouter
}

func NewWaiter(rpcClient *rpc.Client, wsURL string, rpcURL string, routers ...WSRouter) *Waiter {
	var router WSRouter
	if len(routers) > 0 {
		router = routers[0]
	}
	return &Waiter{
		rpc:   rpcClient,
		wsURL: deriveWSURL(wsURL, rpcURL),
		log:   "chainconfirm",
		rt:    router,
	}
}

func (w *Waiter) WaitForConfirmed(ctx context.Context, signature solana.Signature) (Result, error) {
	if w == nil || w.rpc == nil {
		return Result{}, fmt.Errorf("chainconfirm rpc client is not configured")
	}
	logging.LogWS(w.log, ctx, zerolog.DebugLevel, "chain confirm wait start", map[string]any{
		"signature": signature.String(),
		"ws_url":    w.wsURL,
	})
	if status, ok, err := w.checkStatus(ctx, signature); err == nil && ok {
		logging.LogWS(w.log, ctx, zerolog.DebugLevel, "chain confirm http status hit", map[string]any{
			"signature": signature.String(),
			"slot":      status.Slot,
			"status":    status.ConfirmationStatus,
		})
		return status, nil
	}
	if w.rt != nil {
		res, err := w.waitViaRouter(ctx, signature)
		if err == nil {
			logging.LogWS(w.log, ctx, zerolog.DebugLevel, "chain confirm websocket hit", map[string]any{
				"signature": signature.String(),
				"slot":      res.Slot,
				"status":    res.ConfirmationStatus,
			})
			return res, nil
		}
		logging.LogWS(w.log, ctx, zerolog.WarnLevel, "chain confirm websocket failed; fallback to http polling", map[string]any{
			"signature": signature.String(),
			"error":     err.Error(),
		})
	}
	return w.waitWithHTTPPolling(ctx, signature)
}

func (w *Waiter) waitViaRouter(ctx context.Context, signature solana.Signature) (Result, error) {
	ch := make(chan SignatureResult, 1)
	unsubscribe, err := w.rt.SubscribeSignature(signature.String(), "chainconfirm:"+signature.String(), "confirm_waiter", "confirmed", ch)
	if err != nil {
		return Result{}, err
	}
	defer unsubscribe()
	select {
	case <-ctx.Done():
		return Result{}, ctx.Err()
	case res := <-ch:
		if strings.TrimSpace(res.ErrText) != "" {
			return Result{}, fmt.Errorf("signature %s confirmed with chain error: %s", signature.String(), res.ErrText)
		}
		return Result{
			Signature:          res.Signature,
			Slot:               res.Slot,
			ConfirmationStatus: res.ConfirmationStatus,
		}, nil
	}
}

func (w *Waiter) waitWithHTTPPolling(ctx context.Context, signature solana.Signature) (Result, error) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		status, ok, err := w.checkStatus(ctx, signature)
		if err == nil && ok {
			return status, nil
		}
		if err != nil {
			logging.LogWS(w.log, ctx, zerolog.WarnLevel, "chain confirm http polling failed", map[string]any{
				"signature": signature.String(),
				"error":     err.Error(),
			})
		}
		select {
		case <-ctx.Done():
			if err != nil {
				return Result{}, fmt.Errorf("signature still unconfirmed before timeout; last poll error: %w", err)
			}
			return Result{}, ctx.Err()
		case <-ticker.C:
		}
	}
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
