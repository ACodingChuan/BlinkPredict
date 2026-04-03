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
    id, market_id, market_pda, metadata_cid, metadata_url, collateral_mint,
    title, description, category, image_url, status, outcome, resolution_mode,
    resolution_authority, oracle_feed, oracle_condition, oracle_target_price, oracle_target_expo,
    close_time, resolve_after_time, claim_deadline_time, creator_unclaimed_fee, platform_unclaimed_fee, resolved_at, created_at, updated_at
) VALUES (
    $1, $2, $3, $4, $5, $6,
    $7, $8, $9, $10, $11, $12, $13,
    $14, $15, $16, $17, $18,
    $19, $20, $21, $22, $23, $24, $25, $26
) ON CONFLICT (market_id) DO UPDATE SET
    market_pda = EXCLUDED.market_pda,
    metadata_cid = EXCLUDED.metadata_cid,
    metadata_url = EXCLUDED.metadata_url,
    collateral_mint = EXCLUDED.collateral_mint,
    title = EXCLUDED.title,
    description = EXCLUDED.description,
    category = EXCLUDED.category,
    image_url = EXCLUDED.image_url,
    status = EXCLUDED.status,
    outcome = EXCLUDED.outcome,
    resolution_mode = EXCLUDED.resolution_mode,
    resolution_authority = EXCLUDED.resolution_authority,
    oracle_feed = EXCLUDED.oracle_feed,
    oracle_condition = EXCLUDED.oracle_condition,
    oracle_target_price = EXCLUDED.oracle_target_price,
    oracle_target_expo = EXCLUDED.oracle_target_expo,
    close_time = EXCLUDED.close_time,
    resolve_after_time = EXCLUDED.resolve_after_time,
    claim_deadline_time = EXCLUDED.claim_deadline_time,
    creator_unclaimed_fee = EXCLUDED.creator_unclaimed_fee,
    platform_unclaimed_fee = EXCLUDED.platform_unclaimed_fee,
    resolved_at = EXCLUDED.resolved_at,
    updated_at = EXCLUDED.updated_at
`
	_, err := r.pool.Exec(ctx, q,
		market.ID,
		strconv.FormatUint(market.MarketID, 10),
		market.MarketPDA,
		market.MetadataCID,
		market.MetadataURL,
		market.CollateralMint,
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
		market.Resolution.OracleTargetExpo,
		market.CloseTime,
		market.ResolveAfterTime,
		market.ClaimDeadlineTime,
		market.CreatorUnclaimedFee,
		market.PlatformUnclaimedFee,
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
    id, market_id, market_pda, metadata_cid, metadata_url, collateral_mint,
    title, description, category, image_url, status, outcome, resolution_mode,
    resolution_authority, oracle_feed, oracle_condition, oracle_target_price, oracle_target_expo,
    close_time, resolve_after_time, claim_deadline_time, creator_unclaimed_fee, platform_unclaimed_fee, resolved_at, created_at, updated_at
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
    id, market_id, market_pda, metadata_cid, metadata_url, collateral_mint,
    title, description, category, image_url, status, outcome, resolution_mode,
    resolution_authority, oracle_feed, oracle_condition, oracle_target_price, oracle_target_expo,
    close_time, resolve_after_time, claim_deadline_time, creator_unclaimed_fee, platform_unclaimed_fee, resolved_at, created_at, updated_at
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
	metadata_cid = $5,
	metadata_url = $6,
	collateral_mint = $7,
	status = $8,
    outcome = $9,
    resolution_mode = $10,
    resolution_authority = $11,
    oracle_feed = $12,
    oracle_condition = $13,
    oracle_target_price = $14,
    oracle_target_expo = $15,
    close_time = $16,
    resolve_after_time = $17,
    claim_deadline_time = $18,
    creator_unclaimed_fee = $19,
    platform_unclaimed_fee = $20,
    resolved_at = $21,
    updated_at = $22
WHERE market_id = $23`
	tag, err := r.pool.Exec(ctx, q,
		market.Title,
		market.Description,
		market.Category,
		market.ImageURL,
		market.MetadataCID,
		market.MetadataURL,
		market.CollateralMint,
		string(market.Status),
		string(market.Outcome),
		string(market.Resolution.Mode),
		market.Resolution.Authority,
		market.Resolution.OracleFeed,
		string(market.Resolution.OracleCondition),
		int64(market.Resolution.OracleTarget),
		market.Resolution.OracleTargetExpo,
		market.CloseTime,
		market.ResolveAfterTime,
		market.ClaimDeadlineTime,
		market.CreatorUnclaimedFee,
		market.PlatformUnclaimedFee,
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
		market               Market
		marketIDStr          string
		status               string
		outcome              string
		resolutionMode       string
		oracleCondition      string
		oracleTargetPrice    int64
		oracleTargetExpo     int32
		resolveAfterTime     time.Time
		claimDeadlineTime    pgtype.Timestamptz
		creatorUnclaimedFee  int64
		platformUnclaimedFee int64
		resolvedAt           pgtype.Timestamptz
		resolutionAuthority  string
		oracleFeed           string
	)
	if err := row.Scan(
		&market.ID,
		&marketIDStr,
		&market.MarketPDA,
		&market.MetadataCID,
		&market.MetadataURL,
		&market.CollateralMint,
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
		&oracleTargetExpo,
		&market.CloseTime,
		&resolveAfterTime,
		&claimDeadlineTime,
		&creatorUnclaimedFee,
		&platformUnclaimedFee,
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
		Mode:             parseResolutionMode(resolutionMode),
		Authority:        resolutionAuthority,
		OracleFeed:       oracleFeed,
		OracleCondition:  parseOracleCondition(oracleCondition),
		OracleTargetExpo: oracleTargetExpo,
	}
	if oracleTargetPrice > 0 {
		market.Resolution.OracleTarget = uint64(oracleTargetPrice)
	}
	market.ResolveAfterTime = resolveAfterTime
	if claimDeadlineTime.Valid {
		market.ClaimDeadlineTime = claimDeadlineTime.Time
	}
	market.CreatorUnclaimedFee = creatorUnclaimedFee
	market.PlatformUnclaimedFee = platformUnclaimedFee
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
