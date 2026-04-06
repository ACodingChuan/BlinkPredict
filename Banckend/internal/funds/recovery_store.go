package funds

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const recoveryStateRowID int16 = 1

type RecoveryState struct {
	LastFlushedEventSeq uint64
	Inflight            []*InflightMatch
	PendingTerminals    []*PendingTerminal
	PendingReserves     []PendingReserve
	ProcessedSubmits    []string
	ProcessedDeposits   []string
}

func loadRecoveryState(ctx context.Context, pool *pgxpool.Pool) (*RecoveryState, error) {
	if pool == nil {
		return nil, nil
	}
	var (
		lastSeq             int64
		inflightJSON        []byte
		pendingJSON         []byte
		pendingReservesJSON []byte
		submitsJSON         []byte
		depositsJSON        []byte
	)
	err := pool.QueryRow(ctx, `
		SELECT last_flushed_evt_seq,
		       inflight_json,
		       pending_terminals_json,
		       pending_reserves_json,
		       processed_submits_json,
		       processed_deposits_json
		FROM funds_recovery_state
		WHERE recovery_id = $1
	`, recoveryStateRowID).Scan(&lastSeq, &inflightJSON, &pendingJSON, &pendingReservesJSON, &submitsJSON, &depositsJSON)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("load funds recovery state: %w", err)
	}
	state := &RecoveryState{}
	if lastSeq > 0 {
		state.LastFlushedEventSeq = uint64(lastSeq)
	}
	inflightJSON = normalizeJSONSlice(inflightJSON)
	pendingJSON = normalizeJSONSlice(pendingJSON)
	pendingReservesJSON = normalizeJSONSlice(pendingReservesJSON)
	submitsJSON = normalizeJSONSlice(submitsJSON)
	depositsJSON = normalizeJSONSlice(depositsJSON)
	if err := json.Unmarshal(inflightJSON, &state.Inflight); err != nil {
		return nil, fmt.Errorf("decode funds inflight recovery state: %w", err)
	}
	if err := json.Unmarshal(pendingJSON, &state.PendingTerminals); err != nil {
		return nil, fmt.Errorf("decode funds pending recovery state: %w", err)
	}
	if err := json.Unmarshal(pendingReservesJSON, &state.PendingReserves); err != nil {
		return nil, fmt.Errorf("decode funds pending reserve recovery state: %w", err)
	}
	if err := json.Unmarshal(submitsJSON, &state.ProcessedSubmits); err != nil {
		return nil, fmt.Errorf("decode funds submit recovery state: %w", err)
	}
	if err := json.Unmarshal(depositsJSON, &state.ProcessedDeposits); err != nil {
		return nil, fmt.Errorf("decode funds deposit recovery state: %w", err)
	}
	return state, nil
}

func normalizeJSONSlice(raw []byte) []byte {
	if len(raw) == 0 {
		return []byte("[]")
	}
	return raw
}
