package usage

// MeasurementPresence describes fields actually present in upstream usage.
// Zero values alone never prove a measurement was reported.
type MeasurementPresence struct {
	Input        bool `json:"input"`
	Output       bool `json:"output"`
	CacheRead    bool `json:"cache_read"`
	CacheWrite   bool `json:"cache_write"`
	Reasoning    bool `json:"reasoning"`
	Total        bool `json:"total"`
	CacheWrite5m bool `json:"cache_write_5m"`
	CacheWrite1h bool `json:"cache_write_1h"`
}
