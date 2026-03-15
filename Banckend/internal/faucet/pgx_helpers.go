package faucet

import "github.com/jackc/pgx/v5"

func isNoRows(err error) bool {
	return err == pgx.ErrNoRows
}

