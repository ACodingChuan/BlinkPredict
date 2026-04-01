package webhooks

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"blinkpredict/banckend/internal/matching"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
	"github.com/rs/zerolog"
)

type DepositProjector struct {
	pool   *pgxpool.Pool
	redis  *redis.Client
	wallet *matching.SharedWalletManager
	log    *zerolog.Logger
}

type walletAccountProjection struct {
	CollateralTotalUnits uint64
	CollateralFreeUnits  uint64
}

func NewDepositProjector(pool *pgxpool.Pool, redisClient *redis.Client, wallet *matching.SharedWalletManager, logger *zerolog.Logger) *DepositProjector {
	return &DepositProjector{
		pool:   pool,
		redis:  redisClient,
		wallet: wallet,
		log:    logger,
	}
}

func (p *DepositProjector) ApplyDeposit(ctx context.Context, payload DepositSettledPayload) error {
	if p == nil {
		return nil
	}
	if strings.TrimSpace(payload.WalletAddress) == "" {
		return fmt.Errorf("wallet_address is required")
	}
	if payload.AmountUnits == 0 {
		return fmt.Errorf("amount_units must be greater than 0")
	}
	if p.pool == nil {
		return fmt.Errorf("db pool not configured")
	}

	snapshot, inserted, err := p.persistDeposit(ctx, payload)
	if err != nil {
		return err
	}
	if err := p.refreshWalletAccountCache(ctx, payload.WalletAddress, snapshot); err != nil {
		return err
	}
	if inserted && p.wallet != nil {
		p.wallet.ApplyDeposit(payload.WalletAddress, payload.AmountUnits)
	}
	return nil
}

func (p *DepositProjector) persistDeposit(ctx context.Context, payload DepositSettledPayload) (walletAccountProjection, bool, error) {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return walletAccountProjection{}, false, fmt.Errorf("begin deposit projection tx: %w", err)
	}
	defer func() {
		_ = tx.Rollback(ctx)
	}()

	payloadJSON, err := json.Marshal(payload)
	if err != nil {
		return walletAccountProjection{}, false, fmt.Errorf("marshal deposit payload: %w", err)
	}
	eventID := buildHeliusDepositEventID(payload)
	insertResult, err := tx.Exec(ctx, `
		INSERT INTO webhook_receipts (
			event_id, provider, event_type, signature, slot, received_at, payload_json
		) VALUES ($1, $2, $3, $4, $5, NOW(), $6::jsonb)
		ON CONFLICT (event_id) DO NOTHING
	`, eventID, ProviderHelius, EventTypeDepositSettled, nullableString(payload.Signature), int64(payload.Slot), string(payloadJSON))
	if err != nil {
		return walletAccountProjection{}, false, fmt.Errorf("insert webhook receipt: %w", err)
	}
	inserted := insertResult.RowsAffected() > 0

	if inserted {
		confirmedAt := eventTimestamp(payload.BlockTime)
		_, err = tx.Exec(ctx, `
			INSERT INTO deposit_requests (
				id, wallet_address, amount_units, mint, treasury_destination,
				chain_signature, status, source, created_at, confirmed_at
			) VALUES ($1, $2, $3, $4, $5, $6, 'confirmed', 'helius', $7, $7)
			ON CONFLICT (chain_signature) DO UPDATE SET
				wallet_address = EXCLUDED.wallet_address,
				amount_units = EXCLUDED.amount_units,
				mint = EXCLUDED.mint,
				treasury_destination = EXCLUDED.treasury_destination,
				status = EXCLUDED.status,
				source = EXCLUDED.source,
				confirmed_at = EXCLUDED.confirmed_at
		`, uuid.NewString(), payload.WalletAddress, int64(payload.AmountUnits), payload.Mint, payload.ToTokenAccount, nullableString(payload.Signature), confirmedAt)
		if err != nil {
			return walletAccountProjection{}, false, fmt.Errorf("upsert deposit request: %w", err)
		}

		_, err = tx.Exec(ctx, `
			INSERT INTO wallet_accounts (wallet_address, collateral_total_units, collateral_free_units, updated_at)
			VALUES ($1, $2, $2, NOW())
			ON CONFLICT (wallet_address) DO UPDATE SET
				collateral_total_units = wallet_accounts.collateral_total_units + EXCLUDED.collateral_total_units,
				collateral_free_units = wallet_accounts.collateral_free_units + EXCLUDED.collateral_free_units,
				updated_at = NOW()
		`, payload.WalletAddress, int64(payload.AmountUnits))
		if err != nil {
			return walletAccountProjection{}, false, fmt.Errorf("upsert wallet account: %w", err)
		}
	}

	snapshot, err := p.loadWalletAccountProjectionTx(ctx, tx, payload.WalletAddress)
	if err != nil {
		return walletAccountProjection{}, false, err
	}

	if err := tx.Commit(ctx); err != nil {
		return walletAccountProjection{}, false, fmt.Errorf("commit deposit projection tx: %w", err)
	}
	return snapshot, inserted, nil
}

func (p *DepositProjector) loadWalletAccountProjectionTx(ctx context.Context, tx pgx.Tx, walletAddress string) (walletAccountProjection, error) {
	row := tx.QueryRow(ctx, `
		SELECT collateral_total_units, collateral_free_units
		FROM wallet_accounts
		WHERE wallet_address = $1
	`, walletAddress)
	var snapshot walletAccountProjection
	if err := row.Scan(&snapshot.CollateralTotalUnits, &snapshot.CollateralFreeUnits); err != nil {
		return walletAccountProjection{}, fmt.Errorf("load wallet account projection: %w", err)
	}
	return snapshot, nil
}

func (p *DepositProjector) refreshWalletAccountCache(ctx context.Context, walletAddress string, snapshot walletAccountProjection) error {
	if p.redis == nil {
		return nil
	}
	cacheKey := fmt.Sprintf("wallet-account:%s", walletAddress)
	lockedUnits, err := p.redis.HGet(ctx, cacheKey, "collateral_locked_units").Uint64()
	if err != nil && err != redis.Nil {
		return fmt.Errorf("load locked wallet cache: %w", err)
	}

	effectiveFree := snapshot.CollateralFreeUnits
	if lockedUnits > 0 {
		if effectiveFree > lockedUnits {
			effectiveFree -= lockedUnits
		} else {
			effectiveFree = 0
		}
	}

	if err := p.redis.HSet(ctx, cacheKey, map[string]any{
		"collateral_total_units": snapshot.CollateralTotalUnits,
		"collateral_free_units":  effectiveFree,
		"updated_at":             time.Now().UTC().Unix(),
	}).Err(); err != nil {
		return fmt.Errorf("refresh wallet account cache: %w", err)
	}
	return nil
}

func buildHeliusDepositEventID(payload DepositSettledPayload) string {
	return strings.Join([]string{
		ProviderHelius,
		strings.TrimSpace(payload.Signature),
		strings.TrimSpace(payload.Mint),
		strings.TrimSpace(payload.WalletAddress),
		strings.TrimSpace(payload.FromTokenAccount),
		strings.TrimSpace(payload.ToTokenAccount),
		fmt.Sprintf("%d", payload.AmountUnits),
		"deposit",
	}, ":")
}

func eventTimestamp(unixTS int64) time.Time {
	if unixTS <= 0 {
		return time.Now().UTC()
	}
	return time.Unix(unixTS, 0).UTC()
}

func nullableString(value string) any {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	return value
}
