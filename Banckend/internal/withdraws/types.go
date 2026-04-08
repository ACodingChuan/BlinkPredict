package withdraws

type SubmitWithdrawRequest struct {
	Signature     string `json:"signature"`
	WalletAddress string `json:"wallet_address"`
	AmountUnits   uint64 `json:"amount_units"`
}

type SubmitWithdrawResponse struct {
	Status        string `json:"status"`
	Signature     string `json:"signature"`
	WalletAddress string `json:"wallet_address"`
	AmountUnits   uint64 `json:"amount_units"`
}
