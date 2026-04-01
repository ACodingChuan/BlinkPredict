package markets

import "time"

type ResolutionMode string

type OracleCondition string

type MarketStatus string

type MarketOutcome string

const (
	ResolutionModeCreator ResolutionMode = "creator"
	ResolutionModePyth    ResolutionMode = "pyth"

	OracleConditionGT  OracleCondition = "gt"
	OracleConditionGTE OracleCondition = "gte"
	OracleConditionLT  OracleCondition = "lt"
	OracleConditionLTE OracleCondition = "lte"

	MarketStatusOpen     MarketStatus = "open"
	MarketStatusResolved MarketStatus = "resolved"

	MarketOutcomeYes       MarketOutcome = "yes"
	MarketOutcomeNo        MarketOutcome = "no"
	MarketOutcomeUndecided MarketOutcome = "undecided"
)

type ResolutionConfig struct {
	Mode             ResolutionMode  `json:"mode"`
	Authority        string          `json:"authority,omitempty"`
	OracleFeed       string          `json:"oracle_feed,omitempty"`
	OracleCondition  OracleCondition `json:"oracle_condition,omitempty"`
	OracleTarget     uint64          `json:"oracle_target_price,omitempty"`
	OracleTargetExpo int32           `json:"oracle_target_expo,omitempty"`
}

type Market struct {
	ID                   string           `json:"id"`
	MarketID             uint64           `json:"market_id,string"`
	MarketPDA            string           `json:"market_pda"`
	MetadataCID          string           `json:"metadata_cid,omitempty"`
	MetadataURL          string           `json:"metadata_url"`
	CollateralMint       string           `json:"collateral_mint,omitempty"`
	Title                string           `json:"title"`
	Description          string           `json:"description"`
	Category             string           `json:"category"`
	ImageURL             string           `json:"image_url"`
	Status               MarketStatus     `json:"status"`
	Outcome              MarketOutcome    `json:"outcome"`
	Resolution           ResolutionConfig `json:"resolution"`
	CloseTime            time.Time        `json:"close_time"`
	ResolveAfterTime     time.Time        `json:"resolve_after_time"`
	ClaimDeadlineTime    time.Time        `json:"claim_deadline_time"`
	CreatorUnclaimedFee  int64            `json:"creator_unclaimed_fee"`
	PlatformUnclaimedFee int64            `json:"platform_unclaimed_fee"`
	ResolvedAt           *time.Time       `json:"resolved_at,omitempty"`
	CreatedAt            time.Time        `json:"created_at"`
	UpdatedAt            time.Time        `json:"updated_at"`
}

type CreateMarketRequest struct {
	Title             string           `json:"title"`
	Description       string           `json:"description"`
	Category          string           `json:"category"`
	ImageURL          string           `json:"image_url"`
	MetadataCID       string           `json:"metadata_cid"`
	MetadataURL       string           `json:"metadata_url"`
	CloseTime         time.Time        `json:"close_time"`
	ResolveAfterTime  time.Time        `json:"resolve_after_time"`
	ClaimDeadlineTime time.Time        `json:"claim_deadline_time"`
	Resolution        ResolutionConfig `json:"resolution"`
}

type ResolveMarketRequest struct {
	Outcome MarketOutcome `json:"outcome"`
}
