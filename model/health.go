package model

type HealthInfo struct {
	Status    string `json:"status"`
	Timestamp string `json:"timestamp"`
}

type OKResponse struct {
	Status string `json:"status"`
}
