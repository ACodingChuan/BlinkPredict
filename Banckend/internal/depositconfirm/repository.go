package depositconfirm

import (
	"context"
	"fmt"
	"strings"

	"blinkpredict/banckend/internal/protocol"

	"github.com/jackc/pgx/v5/pgxpool"
)

type Submission struct {
	Signature     string
	WalletAddress string
	AmountUnits   uint64
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

func (r *Repository) UpsertSubmitted(ctx context.Context, cmd protocol.DepositConfirmCommand) (Submission, error) {
	if r == nil || r.pool == nil {
		return Submission{}, fmt.Errorf("deposit submissions repository is not configured")
	}
	row := r.pool.QueryRow(ctx, `
		INSERT INTO deposit_submissions (signature, wallet_address, amount_units, status, created_at, updated_at)
		VALUES ($1, $2, $3, 'submitted', NOW(), NOW())
		ON CONFLICT (signature) DO UPDATE SET
			updated_at = NOW()
		RETURNING signature, wallet_address, amount_units, status, failure_reason, slot
	`, cmd.Signature, cmd.WalletAddress, int64(cmd.AmountUnits))
	return scanSubmission(row)
}

func (r *Repository) Load(ctx context.Context, signature string) (Submission, error) {
	row := r.pool.QueryRow(ctx, `
		SELECT signature, wallet_address, amount_units, status, failure_reason, slot
		FROM deposit_submissions
		WHERE signature = $1
	`, strings.TrimSpace(signature))
	return scanSubmission(row)
}

func (r *Repository) MarkWatching(ctx context.Context, signature string) error {
	_, err := r.pool.Exec(ctx, `
		UPDATE deposit_submissions
		SET status = 'watching', updated_at = NOW()
		WHERE signature = $1 AND status IN ('submitted', 'watching')
	`, strings.TrimSpace(signature))
	return err
}

func (r *Repository) MarkConfirmed(ctx context.Context, signature string, slot uint64) error {
	_, err := r.pool.Exec(ctx, `
		UPDATE deposit_submissions
		SET status = 'confirmed', slot = $2, failure_reason = '', confirmed_at = NOW(), updated_at = NOW()
		WHERE signature = $1
	`, strings.TrimSpace(signature), int64(slot))
	return err
}

func (r *Repository) MarkFailed(ctx context.Context, signature, reason string) error {
	_, err := r.pool.Exec(ctx, `
		UPDATE deposit_submissions
		SET status = 'failed', failure_reason = $2, updated_at = NOW()
		WHERE signature = $1
	`, strings.TrimSpace(signature), strings.TrimSpace(reason))
	return err
}

func (r *Repository) MarkExpired(ctx context.Context, signature, reason string) error {
	_, err := r.pool.Exec(ctx, `
		UPDATE deposit_submissions
		SET status = 'expired', failure_reason = $2, updated_at = NOW()
		WHERE signature = $1
	`, strings.TrimSpace(signature), strings.TrimSpace(reason))
	return err
}

type scanner interface{ Scan(dest ...any) error }

func scanSubmission(row scanner) (Submission, error) {
	var out Submission
	var amountUnits int64
	var slot int64
	if err := row.Scan(&out.Signature, &out.WalletAddress, &amountUnits, &out.Status, &out.FailureReason, &slot); err != nil {
		return Submission{}, err
	}
	if amountUnits > 0 {
		out.AmountUnits = uint64(amountUnits)
	}
	if slot > 0 {
		out.Slot = uint64(slot)
	}
	return out, nil
}
