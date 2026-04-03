package marketconfirm

import (
	"context"
	"fmt"
	"strings"

	"blinkpredict/banckend/internal/protocol"

	"github.com/jackc/pgx/v5/pgxpool"
)

type Submission struct {
	Signature     string
	MarketID      uint64
	MarketPDA     string
	CreatorWallet string
	MetadataCID   string
	Status        string
	FailureReason string
	Slot          uint64
}

type Repository struct {
	pool *pgxpool.Pool
}

func NewRepository(pool *pgxpool.Pool) *Repository {
	return &Repository{pool: pool}
}

func (r *Repository) UpsertSubmitted(ctx context.Context, cmd protocol.MarketConfirmCommand) (Submission, error) {
	if r == nil || r.pool == nil {
		return Submission{}, fmt.Errorf("market submissions repository is not configured")
	}
	row := r.pool.QueryRow(ctx, `
		INSERT INTO market_submissions (signature, status, created_at, updated_at)
		VALUES ($1, 'submitted', NOW(), NOW())
		ON CONFLICT (signature) DO UPDATE SET
			updated_at = NOW()
		RETURNING signature, market_id, market_pda, creator_wallet, metadata_cid, status, failure_reason, slot
	`, strings.TrimSpace(cmd.Signature))
	return scanSubmission(row)
}

func (r *Repository) Load(ctx context.Context, signature string) (Submission, error) {
	row := r.pool.QueryRow(ctx, `
		SELECT signature, market_id, market_pda, creator_wallet, metadata_cid, status, failure_reason, slot
		FROM market_submissions
		WHERE signature = $1
	`, strings.TrimSpace(signature))
	return scanSubmission(row)
}

func (r *Repository) MarkWatching(ctx context.Context, signature string) error {
	_, err := r.pool.Exec(ctx, `
		UPDATE market_submissions
		SET status = 'watching', updated_at = NOW()
		WHERE signature = $1 AND status IN ('submitted', 'watching')
	`, strings.TrimSpace(signature))
	return err
}

func (r *Repository) MarkConfirmed(ctx context.Context, event protocol.MarketConfirmedEvent) error {
	_, err := r.pool.Exec(ctx, `
		UPDATE market_submissions
		SET status = 'confirmed',
			market_id = $2,
			market_pda = $3,
			creator_wallet = $4,
			metadata_cid = $5,
			slot = $6,
			failure_reason = '',
			confirmed_at = NOW(),
			updated_at = NOW()
		WHERE signature = $1
	`, strings.TrimSpace(event.Signature), uint64ToNumericString(event.MarketID), strings.TrimSpace(event.MarketPDA), strings.TrimSpace(event.Creator), strings.TrimSpace(event.MetadataCID), int64(event.Slot))
	return err
}

func (r *Repository) MarkFailed(ctx context.Context, signature, reason string) error {
	_, err := r.pool.Exec(ctx, `
		UPDATE market_submissions
		SET status = 'failed', failure_reason = $2, updated_at = NOW()
		WHERE signature = $1
	`, strings.TrimSpace(signature), strings.TrimSpace(reason))
	return err
}

func (r *Repository) MarkExpired(ctx context.Context, signature, reason string) error {
	_, err := r.pool.Exec(ctx, `
		UPDATE market_submissions
		SET status = 'expired', failure_reason = $2, updated_at = NOW()
		WHERE signature = $1
	`, strings.TrimSpace(signature), strings.TrimSpace(reason))
	return err
}

type scanner interface{ Scan(dest ...any) error }

func scanSubmission(row scanner) (Submission, error) {
	var out Submission
	var marketIDStr *string
	var slot int64
	if err := row.Scan(&out.Signature, &marketIDStr, &out.MarketPDA, &out.CreatorWallet, &out.MetadataCID, &out.Status, &out.FailureReason, &slot); err != nil {
		return Submission{}, err
	}
	if marketIDStr != nil && strings.TrimSpace(*marketIDStr) != "" {
		var marketID uint64
		_, err := fmt.Sscan(strings.TrimSpace(*marketIDStr), &marketID)
		if err != nil {
			return Submission{}, fmt.Errorf("parse market_id: %w", err)
		}
		out.MarketID = marketID
	}
	if slot > 0 {
		out.Slot = uint64(slot)
	}
	return out, nil
}

func uint64ToNumericString(v uint64) string {
	return fmt.Sprintf("%d", v)
}
