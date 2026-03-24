package markets

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

type PostgresRepository struct {
	pool *pgxpool.Pool
}

func NewPostgresRepository(pool *pgxpool.Pool) *PostgresRepository {
	return &PostgresRepository{pool: pool}
}

func (r *PostgresRepository) Save(ctx context.Context, market Market) error {
	const q = `
INSERT INTO markets (
    id, market_id, market_pda, metadata_url, collateral_mint, collateral_vault, yes_mint, no_mint,
    title, description, category, image_url, status, outcome, resolution_mode,
    resolution_authority, oracle_feed, oracle_condition, oracle_target_price, oracle_observation_time,
    close_time, resolved_at, created_at, updated_at
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8,
    $9, $10, $11, $12, $13, $14, $15,
    $16, $17, $18, $19, $20,
    $21, $22, $23, $24
)`
	_, err := r.pool.Exec(ctx, q,
		market.ID,
		strconv.FormatUint(market.MarketID, 10),
		market.MarketPDA,
		market.MetadataURL,
		market.CollateralMint,
		market.CollateralVault,
		market.YesMint,
		market.NoMint,
		market.Title,
		market.Description,
		market.Category,
		market.ImageURL,
		string(market.Status),
		string(market.Outcome),
		string(market.Resolution.Mode),
		market.Resolution.Authority,
		market.Resolution.OracleFeed,
		string(market.Resolution.OracleCondition),
		int64(market.Resolution.OracleTarget),
		nullableTime(market.Resolution.ObservationTime),
		market.CloseTime,
		market.ResolvedAt,
		market.CreatedAt,
		market.UpdatedAt,
	)
	if err != nil {
		return fmt.Errorf("insert market: %w", err)
	}
	return nil
}

func (r *PostgresRepository) List(ctx context.Context) ([]Market, error) {
	const q = `
SELECT
    id, market_id, market_pda, metadata_url, collateral_mint, collateral_vault, yes_mint, no_mint,
    title, description, category, image_url, status, outcome, resolution_mode,
    resolution_authority, oracle_feed, oracle_condition, oracle_target_price, oracle_observation_time,
    close_time, resolved_at, created_at, updated_at
FROM markets
ORDER BY created_at DESC`

	rows, err := r.pool.Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("list markets: %w", err)
	}
	defer rows.Close()

	items := make([]Market, 0)
	for rows.Next() {
		market, scanErr := scanMarket(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		items = append(items, market)
	}
	if rows.Err() != nil {
		return nil, fmt.Errorf("iterate markets: %w", rows.Err())
	}
	return items, nil
}

func (r *PostgresRepository) Get(ctx context.Context, marketID uint64) (Market, error) {
	const q = `
SELECT
    id, market_id, market_pda, metadata_url, collateral_mint, collateral_vault, yes_mint, no_mint,
    title, description, category, image_url, status, outcome, resolution_mode,
    resolution_authority, oracle_feed, oracle_condition, oracle_target_price, oracle_observation_time,
    close_time, resolved_at, created_at, updated_at
FROM markets
WHERE market_id = $1`
	row := r.pool.QueryRow(ctx, q, strconv.FormatUint(marketID, 10))
	market, err := scanMarket(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Market{}, ErrMarketNotFound
		}
		return Market{}, err
	}
	return market, nil
}

func (r *PostgresRepository) Update(ctx context.Context, market Market) error {
	const q = `
UPDATE markets
SET
    title = $1,
    description = $2,
    category = $3,
    image_url = $4,
    metadata_url = $5,
    collateral_mint = $6,
    status = $7,
    outcome = $8,
    resolution_mode = $9,
    resolution_authority = $10,
    oracle_feed = $11,
    oracle_condition = $12,
    oracle_target_price = $13,
    oracle_observation_time = $14,
    close_time = $15,
    resolved_at = $16,
    updated_at = $17
WHERE market_id = $18`
	tag, err := r.pool.Exec(ctx, q,
		market.Title,
		market.Description,
		market.Category,
		market.ImageURL,
		market.MetadataURL,
		market.CollateralMint,
		string(market.Status),
		string(market.Outcome),
		string(market.Resolution.Mode),
		market.Resolution.Authority,
		market.Resolution.OracleFeed,
		string(market.Resolution.OracleCondition),
		int64(market.Resolution.OracleTarget),
		nullableTime(market.Resolution.ObservationTime),
		market.CloseTime,
		market.ResolvedAt,
		market.UpdatedAt,
		strconv.FormatUint(market.MarketID, 10),
	)
	if err != nil {
		return fmt.Errorf("update market: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrMarketNotFound
	}
	return nil
}

func scanMarket(row interface {
	Scan(dest ...any) error
}) (Market, error) {
	var (
		market                Market
		marketIDStr           string
		status                string
		outcome               string
		resolutionMode        string
		oracleCondition       string
		oracleTargetPrice     int64
		oracleObservationTime pgtype.Timestamptz
		resolvedAt            pgtype.Timestamptz
		resolutionAuthority   string
		oracleFeed            string
	)
	if err := row.Scan(
		&market.ID,
		&marketIDStr,
		&market.MarketPDA,
		&market.MetadataURL,
		&market.CollateralMint,
		&market.CollateralVault,
		&market.YesMint,
		&market.NoMint,
		&market.Title,
		&market.Description,
		&market.Category,
		&market.ImageURL,
		&status,
		&outcome,
		&resolutionMode,
		&resolutionAuthority,
		&oracleFeed,
		&oracleCondition,
		&oracleTargetPrice,
		&oracleObservationTime,
		&market.CloseTime,
		&resolvedAt,
		&market.CreatedAt,
		&market.UpdatedAt,
	); err != nil {
		return Market{}, err
	}

	parsedID, err := strconv.ParseUint(marketIDStr, 10, 64)
	if err != nil {
		return Market{}, fmt.Errorf("invalid market_id from db: %s", marketIDStr)
	}
	market.MarketID = parsedID
	market.Status = parseStatus(status)
	market.Outcome = parseOutcome(outcome)
	market.Resolution = ResolutionConfig{
		Mode:            parseResolutionMode(resolutionMode),
		Authority:       resolutionAuthority,
		OracleFeed:      oracleFeed,
		OracleCondition: parseOracleCondition(oracleCondition),
	}
	if oracleTargetPrice > 0 {
		market.Resolution.OracleTarget = uint64(oracleTargetPrice)
	}
	if oracleObservationTime.Valid {
		market.Resolution.ObservationTime = oracleObservationTime.Time
	}
	if resolvedAt.Valid {
		t := resolvedAt.Time
		market.ResolvedAt = &t
	}
	return market, nil
}

func nullableTime(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

func parseStatus(raw string) MarketStatus {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case string(MarketStatusResolved):
		return MarketStatusResolved
	default:
		return MarketStatusOpen
	}
}

func parseOutcome(raw string) MarketOutcome {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case string(MarketOutcomeYes):
		return MarketOutcomeYes
	case string(MarketOutcomeNo):
		return MarketOutcomeNo
	default:
		return MarketOutcomeUndecided
	}
}

func parseResolutionMode(raw string) ResolutionMode {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case string(ResolutionModePyth):
		return ResolutionModePyth
	default:
		return ResolutionModeCreator
	}
}

func parseOracleCondition(raw string) OracleCondition {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case string(OracleConditionGT):
		return OracleConditionGT
	case string(OracleConditionLT):
		return OracleConditionLT
	case string(OracleConditionLTE):
		return OracleConditionLTE
	default:
		return OracleConditionGTE
	}
}

var _ Repository = (*PostgresRepository)(nil)
