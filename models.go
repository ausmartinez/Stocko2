package main

// Position represents a single stock holding
type Position struct {
	Symbol   string  `json:"symbol"`
	Shares   float64 `json:"shares"`
	BuyPrice float64 `json:"buy_price"`
	BoughtAt string  `json:"bought_at"`
}

// Config represents the structure of our JSON file
type Config struct {
	AppName     string     `json:"app_name"`
	Version     string     `json:"version"`
	Debug       bool       `json:"debug"`
	MaxDaySpend int        `json:"max_day_spend"`
	CashFloor   int        `json:"CashFloor"`
	Positions   []Position `json:"positions"`
}
