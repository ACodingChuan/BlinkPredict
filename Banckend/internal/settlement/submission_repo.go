package settlement

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"blinkpredict/banckend/internal/matching"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type MarketLaneStatus string

const (
	LaneActive MarketLaneStatus = "active"
	LanePaused MarketLaneStatus = "paused"
)

type SubmissionStatus string

const (
	StatusQueued    SubmissionStatus = "queued"
	StatusPrepared  SubmissionStatus = "prepared"
	StatusSubmitted SubmissionStatus = "submitted"
	StatusProcessed SubmissionStatus = "processed"
	StatusConfirmed SubmissionStatus = "confirmed"
	StatusFailed    SubmissionStatus = "failed"
)

type SubmissionRecord struct {
	MatchEventID            string
	MarketID                uint64
	MarketPDA               string
	Status                  string
	MarketLaneStatus        string
	MatchEventJSON          []byte
	PreparedPayload         []byte
	Wallets                 []string
	TxSignature             string
	RawTxBase64             string
	LastValidBlockHeight    uint64
	RetryCount              int
	ProcessedSlot           uint64
	ConfirmationSlot        uint64
	ReasonCode              string
	SubmittedEventPublished bool
	TerminalEventPublished  bool
	PreparedAt              time.Time
	ProcessedAt             time.Time
	ConfirmedAt             time.Time
	Version                 int64
	CreatedAt               time.Time
	UpdatedAt               time.Time
}

type queuedInsert struct {
	MatchEventID     string
	MarketID         uint64
	MarketPDA        string
	MatchEventJSON   []byte
	WalletsJSON      []byte
	MarketLaneStatus string
}

type submissionRepo struct {
	pool *pgxpool.Pool
}

func newSubmissionRepo(pool *pgxpool.Pool) *submissionRepo {
	return &submissionRepo{pool: pool}
}

func (r *submissionRepo) UpsertQueuedBatch(ctx context.Context, rows []queuedInsert) ([]bool, error) {
	if r == nil || r.pool == nil || len(rows) == 0 {
		return nil, nil
	}
	inserted := make([]bool, len(rows))
	batch := &pgx.Batch{}
	for _, row := range rows {
		batch.Queue(`
			INSERT INTO settlement_submissions (
				match_event_id,
				market_id,
				market_pda,
				status,
				market_lane_status,
				match_event_json,
				wallets_json,
				version,
				created_at,
				updated_at
			) VALUES (
				$1,
				$2::NUMERIC(20,0),
				$3,
				'queued',
				$4,
				$5::jsonb,
				$6::jsonb,
				1,
				NOW(),
				NOW()
			)
			ON CONFLICT (match_event_id) DO NOTHING
			RETURNING match_event_id
		`, row.MatchEventID, strconv.FormatUint(row.MarketID, 10), row.MarketPDA, row.MarketLaneStatus, row.MatchEventJSON, row.WalletsJSON)
	}
	results := r.pool.SendBatch(ctx, batch)
	defer results.Close()
	for i := range rows {
		var matchEventID string
		err := results.QueryRow().Scan(&matchEventID)
		if err == nil {
			inserted[i] = true
			continue
		}
		if err == pgx.ErrNoRows {
			inserted[i] = false
			continue
		}
		return nil, fmt.Errorf("upsert settlement_submissions queued: %w", err)
	}
	return inserted, nil
}

func (r *submissionRepo) LoadNextQueuedByMarket(ctx context.Context, marketID uint64) (SubmissionRecord, bool, error) {
	row := r.pool.QueryRow(ctx, `
		SELECT
			match_event_id,
			market_id::TEXT,
			market_pda,
			status,
			market_lane_status,
			match_event_json,
			prepared_payload,
			wallets_json,
			tx_signature,
			raw_tx_base64,
			last_valid_block_height,
			retry_count,
			processed_slot,
			confirmation_slot,
			reason_code,
			submitted_event_published,
			terminal_event_published,
			prepared_at,
			processed_at,
			confirmed_at,
			version,
			created_at,
			updated_at
		FROM settlement_submissions
		WHERE market_id = $1::NUMERIC(20,0)
		  AND status = 'queued'
		  AND market_lane_status = 'active'
		ORDER BY created_at ASC
		LIMIT 1
	`, strconv.FormatUint(marketID, 10))
	record, err := scanSubmissionRecord(row)
	if err != nil {
		if err == pgx.ErrNoRows {
			return SubmissionRecord{}, false, nil
		}
		return SubmissionRecord{}, false, err
	}
	return record, true, nil
}

func (r *submissionRepo) LoadNextPreparedByMarket(ctx context.Context, marketID uint64) (SubmissionRecord, bool, error) {
	row := r.pool.QueryRow(ctx, `
		SELECT
			match_event_id,
			market_id::TEXT,
			market_pda,
			status,
			market_lane_status,
			match_event_json,
			prepared_payload,
			wallets_json,
			tx_signature,
			raw_tx_base64,
			last_valid_block_height,
			retry_count,
			processed_slot,
			confirmation_slot,
			reason_code,
			submitted_event_published,
			terminal_event_published,
			prepared_at,
			processed_at,
			confirmed_at,
			version,
			created_at,
			updated_at
		FROM settlement_submissions
		WHERE market_id = $1::NUMERIC(20,0)
		  AND status = 'prepared'
		  AND market_lane_status = 'active'
		ORDER BY created_at ASC
		LIMIT 1
	`, strconv.FormatUint(marketID, 10))
	record, err := scanSubmissionRecord(row)
	if err != nil {
		if err == pgx.ErrNoRows {
			return SubmissionRecord{}, false, nil
		}
		return SubmissionRecord{}, false, err
	}
	return record, true, nil
}

func (r *submissionRepo) LoadByMatchEventID(ctx context.Context, matchEventID string) (SubmissionRecord, bool, error) {
	row := r.pool.QueryRow(ctx, `
		SELECT
			match_event_id,
			market_id::TEXT,
			market_pda,
			status,
			market_lane_status,
			match_event_json,
			prepared_payload,
			wallets_json,
			tx_signature,
			raw_tx_base64,
			last_valid_block_height,
			retry_count,
			processed_slot,
			confirmation_slot,
			reason_code,
			submitted_event_published,
			terminal_event_published,
			prepared_at,
			processed_at,
			confirmed_at,
			version,
			created_at,
			updated_at
		FROM settlement_submissions
		WHERE match_event_id = $1
	`, strings.TrimSpace(matchEventID))
	record, err := scanSubmissionRecord(row)
	if err != nil {
		if err == pgx.ErrNoRows {
			return SubmissionRecord{}, false, nil
		}
		return SubmissionRecord{}, false, err
	}
	return record, true, nil
}

func (r *submissionRepo) MarkSubmittedCAS(ctx context.Context, matchEventID string, txSignature string, rawTx string, lastValidBlockHeight uint64) (bool, error) {
	tag, err := r.pool.Exec(ctx, `
		UPDATE settlement_submissions
		SET tx_signature = $2,
		    raw_tx_base64 = $3,
		    last_valid_block_height = $4,
		    status = 'submitted',
		    version = version + 1,
		    updated_at = NOW()
		WHERE match_event_id = $1
		  AND status = 'prepared'
		  AND market_lane_status = 'active'
	`, strings.TrimSpace(matchEventID), strings.TrimSpace(txSignature), strings.TrimSpace(rawTx), int64(lastValidBlockHeight))
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

func (r *submissionRepo) MarkPreparedCAS(ctx context.Context, matchEventID string, preparedPayload []byte) (bool, error) {
	tag, err := r.pool.Exec(ctx, `
		UPDATE settlement_submissions
		SET prepared_payload = $2::jsonb,
		    status = 'prepared',
		    prepared_at = NOW(),
		    version = version + 1,
		    updated_at = NOW()
		WHERE match_event_id = $1
		  AND status = 'queued'
		  AND market_lane_status = 'active'
	`, strings.TrimSpace(matchEventID), preparedPayload)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

func (r *submissionRepo) MarkSubmittedEventPublished(ctx context.Context, matchEventID string) error {
	_, err := r.pool.Exec(ctx, `
		UPDATE settlement_submissions
		SET submitted_event_published = TRUE,
		    version = version + 1,
		    updated_at = NOW()
		WHERE match_event_id = $1
	`, strings.TrimSpace(matchEventID))
	return err
}

func (r *submissionRepo) MarkConfirmedCAS(ctx context.Context, matchEventID string, txSignature string, slot uint64) (bool, error) {
	tag, err := r.pool.Exec(ctx, `
		UPDATE settlement_submissions
		SET status = 'confirmed',
		    confirmation_slot = $3,
		    confirmed_at = NOW(),
		    version = version + 1,
		    updated_at = NOW()
		WHERE match_event_id = $1
		  AND status = 'processed'
		  AND tx_signature = $2
	`, strings.TrimSpace(matchEventID), strings.TrimSpace(txSignature), int64(slot))
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

func (r *submissionRepo) MarkProcessedCAS(ctx context.Context, matchEventID string, txSignature string, slot uint64) (bool, error) {
	tag, err := r.pool.Exec(ctx, `
		UPDATE settlement_submissions
		SET status = 'processed',
		    processed_slot = $3,
		    processed_at = NOW(),
		    version = version + 1,
		    updated_at = NOW()
		WHERE match_event_id = $1
		  AND status = 'submitted'
		  AND tx_signature = $2
	`, strings.TrimSpace(matchEventID), strings.TrimSpace(txSignature), int64(slot))
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

func (r *submissionRepo) MarkFailedBeforeSubmitAndPauseQueued(ctx context.Context, matchEventID string, reasonCode string) (bool, uint64, error) {
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return false, 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var marketIDStr string
	if err := tx.QueryRow(ctx, `
		UPDATE settlement_submissions
		SET status = 'failed',
		    market_lane_status = 'paused',
		    reason_code = $2,
		    version = version + 1,
		    updated_at = NOW()
		WHERE match_event_id = $1
		  AND status IN ('queued', 'prepared')
		RETURNING market_id::TEXT
	`, strings.TrimSpace(matchEventID), strings.TrimSpace(reasonCode)).Scan(&marketIDStr); err != nil {
		if err == pgx.ErrNoRows {
			return false, 0, nil
		}
		return false, 0, err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE settlement_submissions
		SET market_lane_status = 'paused',
		    version = version + 1,
		    updated_at = NOW()
		WHERE market_id = $1::NUMERIC(20,0)
		  AND status IN ('queued', 'prepared')
		  AND market_lane_status = 'active'
	`, marketIDStr); err != nil {
		return false, 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, 0, err
	}
	marketID, err := parseUint64(marketIDStr)
	if err != nil {
		return false, 0, err
	}
	return true, marketID, nil
}

func (r *submissionRepo) MarkFailedAndPauseQueued(ctx context.Context, matchEventID string, txSignature string, reasonCode string) (bool, uint64, error) {
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return false, 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var marketIDStr string
	if err := tx.QueryRow(ctx, `
		UPDATE settlement_submissions
		SET status = 'failed',
		    market_lane_status = 'paused',
		    reason_code = $3,
		    version = version + 1,
		    updated_at = NOW()
		WHERE match_event_id = $1
		  AND status IN ('submitted', 'processed')
		  AND tx_signature = $2
		RETURNING market_id::TEXT
	`, strings.TrimSpace(matchEventID), strings.TrimSpace(txSignature), strings.TrimSpace(reasonCode)).Scan(&marketIDStr); err != nil {
		if err == pgx.ErrNoRows {
			return false, 0, nil
		}
		return false, 0, err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE settlement_submissions
		SET market_lane_status = 'paused',
		    version = version + 1,
		    updated_at = NOW()
		WHERE market_id = $1::NUMERIC(20,0)
		  AND status IN ('queued', 'prepared')
		  AND market_lane_status = 'active'
	`, marketIDStr); err != nil {
		return false, 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, 0, err
	}
	marketID, err := parseUint64(marketIDStr)
	if err != nil {
		return false, 0, err
	}
	return true, marketID, nil
}

func (r *submissionRepo) ReplaceSignatureCAS(ctx context.Context, matchEventID string, oldSignature string, newSignature string, rawTx string, lastValidBlockHeight uint64) (bool, error) {
	tag, err := r.pool.Exec(ctx, `
		UPDATE settlement_submissions
		SET tx_signature = $3,
		    raw_tx_base64 = $4,
		    last_valid_block_height = $5,
		    retry_count = retry_count + 1,
		    version = version + 1,
		    updated_at = NOW()
		WHERE match_event_id = $1
		  AND status = 'submitted'
		  AND tx_signature = $2
	`, strings.TrimSpace(matchEventID), strings.TrimSpace(oldSignature), strings.TrimSpace(newSignature), strings.TrimSpace(rawTx), int64(lastValidBlockHeight))
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

func (r *submissionRepo) MarkTerminalEventPublished(ctx context.Context, matchEventID string) error {
	_, err := r.pool.Exec(ctx, `
		UPDATE settlement_submissions
		SET terminal_event_published = TRUE,
		    version = version + 1,
		    updated_at = NOW()
		WHERE match_event_id = $1
	`, strings.TrimSpace(matchEventID))
	return err
}

func (r *submissionRepo) ListSubmitted(ctx context.Context) ([]SubmissionRecord, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT
			match_event_id,
			market_id::TEXT,
			market_pda,
			status,
			market_lane_status,
			match_event_json,
			prepared_payload,
			wallets_json,
			tx_signature,
			raw_tx_base64,
			last_valid_block_height,
			retry_count,
			processed_slot,
			confirmation_slot,
			reason_code,
			submitted_event_published,
			terminal_event_published,
			prepared_at,
			processed_at,
			confirmed_at,
			version,
			created_at,
			updated_at
		FROM settlement_submissions
		WHERE status = 'submitted'
		ORDER BY created_at ASC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collectSubmissionRecords(rows)
}

func (r *submissionRepo) ListProcessed(ctx context.Context, limit int) ([]SubmissionRecord, error) {
	if limit <= 0 {
		limit = 200
	}
	rows, err := r.pool.Query(ctx, `
		SELECT
			match_event_id,
			market_id::TEXT,
			market_pda,
			status,
			market_lane_status,
			match_event_json,
			prepared_payload,
			wallets_json,
			tx_signature,
			raw_tx_base64,
			last_valid_block_height,
			retry_count,
			processed_slot,
			confirmation_slot,
			reason_code,
			submitted_event_published,
			terminal_event_published,
			prepared_at,
			processed_at,
			confirmed_at,
			version,
			created_at,
			updated_at
		FROM settlement_submissions
		WHERE status = 'processed'
		ORDER BY COALESCE(processed_at, updated_at, created_at) ASC
		LIMIT $1
	`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collectSubmissionRecords(rows)
}

func (r *submissionRepo) ListUnpublishedSubmitted(ctx context.Context) ([]SubmissionRecord, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT
			match_event_id,
			market_id::TEXT,
			market_pda,
			status,
			market_lane_status,
			match_event_json,
			prepared_payload,
			wallets_json,
			tx_signature,
			raw_tx_base64,
			last_valid_block_height,
			retry_count,
			processed_slot,
			confirmation_slot,
			reason_code,
			submitted_event_published,
			terminal_event_published,
			prepared_at,
			processed_at,
			confirmed_at,
			version,
			created_at,
			updated_at
		FROM settlement_submissions
		WHERE status = 'submitted'
		  AND submitted_event_published = FALSE
		ORDER BY created_at ASC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collectSubmissionRecords(rows)
}

func (r *submissionRepo) ListUnpublishedTerminal(ctx context.Context) ([]SubmissionRecord, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT
			match_event_id,
			market_id::TEXT,
			market_pda,
			status,
			market_lane_status,
			match_event_json,
			prepared_payload,
			wallets_json,
			tx_signature,
			raw_tx_base64,
			last_valid_block_height,
			retry_count,
			processed_slot,
			confirmation_slot,
			reason_code,
			submitted_event_published,
			terminal_event_published,
			prepared_at,
			processed_at,
			confirmed_at,
			version,
			created_at,
			updated_at
		FROM settlement_submissions
		WHERE status IN ('confirmed', 'failed')
		  AND terminal_event_published = FALSE
		ORDER BY updated_at ASC, created_at ASC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collectSubmissionRecords(rows)
}

func (r *submissionRepo) ListQueuedMarketIDs(ctx context.Context) ([]uint64, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT DISTINCT market_id::TEXT
		FROM settlement_submissions
		WHERE status = 'queued'
		  AND market_lane_status = 'active'
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]uint64, 0)
	for rows.Next() {
		var marketIDStr string
		if err := rows.Scan(&marketIDStr); err != nil {
			return nil, err
		}
		marketID, err := parseUint64(marketIDStr)
		if err != nil {
			return nil, err
		}
		out = append(out, marketID)
	}
	return out, rows.Err()
}

func (r *submissionRepo) ListPreparedMarketIDs(ctx context.Context) ([]uint64, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT DISTINCT market_id::TEXT
		FROM settlement_submissions
		WHERE status = 'prepared'
		  AND market_lane_status = 'active'
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]uint64, 0)
	for rows.Next() {
		var marketIDStr string
		if err := rows.Scan(&marketIDStr); err != nil {
			return nil, err
		}
		marketID, err := parseUint64(marketIDStr)
		if err != nil {
			return nil, err
		}
		out = append(out, marketID)
	}
	return out, rows.Err()
}

func (r *submissionRepo) ListPausedMarketIDs(ctx context.Context) ([]uint64, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT DISTINCT market_id::TEXT
		FROM settlement_submissions
		WHERE market_lane_status = 'paused'
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]uint64, 0)
	for rows.Next() {
		var marketIDStr string
		if err := rows.Scan(&marketIDStr); err != nil {
			return nil, err
		}
		marketID, err := parseUint64(marketIDStr)
		if err != nil {
			return nil, err
		}
		out = append(out, marketID)
	}
	return out, rows.Err()
}

type rowScanner interface{ Scan(dest ...any) error }
type rowsScanner interface {
	Next() bool
	Scan(dest ...any) error
	Err() error
}

func scanSubmissionRecord(row rowScanner) (SubmissionRecord, error) {
	var (
		record              SubmissionRecord
		marketIDStr         string
		walletsJSON         []byte
		processedSlot       int64
		confirmationSlot    int64
		height              int64
		preparedAtNullable  *time.Time
		processedAtNullable *time.Time
		confirmedAtNullable *time.Time
	)
	if err := row.Scan(
		&record.MatchEventID,
		&marketIDStr,
		&record.MarketPDA,
		&record.Status,
		&record.MarketLaneStatus,
		&record.MatchEventJSON,
		&record.PreparedPayload,
		&walletsJSON,
		&record.TxSignature,
		&record.RawTxBase64,
		&height,
		&record.RetryCount,
		&processedSlot,
		&confirmationSlot,
		&record.ReasonCode,
		&record.SubmittedEventPublished,
		&record.TerminalEventPublished,
		&preparedAtNullable,
		&processedAtNullable,
		&confirmedAtNullable,
		&record.Version,
		&record.CreatedAt,
		&record.UpdatedAt,
	); err != nil {
		return SubmissionRecord{}, err
	}
	marketID, err := parseUint64(marketIDStr)
	if err != nil {
		return SubmissionRecord{}, err
	}
	record.MarketID = marketID
	if height > 0 {
		record.LastValidBlockHeight = uint64(height)
	}
	if processedSlot > 0 {
		record.ProcessedSlot = uint64(processedSlot)
	}
	if confirmationSlot > 0 {
		record.ConfirmationSlot = uint64(confirmationSlot)
	}
	if preparedAtNullable != nil {
		record.PreparedAt = preparedAtNullable.UTC()
	}
	if processedAtNullable != nil {
		record.ProcessedAt = processedAtNullable.UTC()
	}
	if confirmedAtNullable != nil {
		record.ConfirmedAt = confirmedAtNullable.UTC()
	}
	if len(walletsJSON) > 0 {
		if err := json.Unmarshal(walletsJSON, &record.Wallets); err != nil {
			return SubmissionRecord{}, fmt.Errorf("decode wallets_json: %w", err)
		}
	}
	return record, nil
}

func collectSubmissionRecords(rows rowsScanner) ([]SubmissionRecord, error) {
	out := make([]SubmissionRecord, 0)
	for rows.Next() {
		record, err := scanSubmissionRecord(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, record)
	}
	return out, rows.Err()
}

func walletsForSubmissionEvent(event matching.MatchBatchEvent) []string {
	seen := make(map[string]struct{})
	out := make([]string, 0, len(event.Orders))
	for _, order := range event.Orders {
		wallet := strings.TrimSpace(order.Execution.WalletAddress)
		if wallet == "" {
			continue
		}
		if _, ok := seen[wallet]; ok {
			continue
		}
		seen[wallet] = struct{}{}
		out = append(out, wallet)
	}
	return out
}

func parseUint64(value string) (uint64, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, nil
	}
	return strconv.ParseUint(value, 10, 64)
}
