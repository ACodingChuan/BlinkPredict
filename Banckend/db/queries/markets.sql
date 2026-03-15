-- name: ListMarkets :many
SELECT * FROM markets ORDER BY created_at DESC;

-- name: GetMarketByID :one
SELECT * FROM markets WHERE market_id = $1;
