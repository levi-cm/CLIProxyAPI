package config

import (
	"fmt"
	"time"
)

// CodexCapacityRetryConfig bounds recovery waits; it never changes transport
// deadlines. Zero-valued optional limits use conservative defaults.
type CodexCapacityRetryConfig struct {
	Enabled                 bool   `yaml:"enabled" json:"enabled"`
	TotalBudget             string `yaml:"total-budget,omitempty" json:"total-budget,omitempty"`
	MaxAttempts             int    `yaml:"max-attempts,omitempty" json:"max-attempts,omitempty"`
	MaxPending              int    `yaml:"max-pending,omitempty" json:"max-pending,omitempty"`
	MaxPendingPerAccount    int    `yaml:"max-pending-per-account,omitempty" json:"max-pending-per-account,omitempty"`
	MaxPendingBytes         int64  `yaml:"max-pending-bytes,omitempty" json:"max-pending-bytes,omitempty"`
	MinAvailableMemoryBytes int64  `yaml:"min-available-memory-bytes,omitempty" json:"min-available-memory-bytes,omitempty"`
}

func (c CodexCapacityRetryConfig) WithDefaults() CodexCapacityRetryConfig {
	if c.TotalBudget == "" {
		c.TotalBudget = "30m"
	}
	if c.MaxAttempts == 0 {
		c.MaxAttempts = 8
	}
	if c.MaxPending == 0 {
		c.MaxPending = 16
	}
	if c.MaxPendingPerAccount == 0 {
		c.MaxPendingPerAccount = 8
	}
	if c.MaxPendingBytes == 0 {
		c.MaxPendingBytes = 128 << 20
	}
	if c.MinAvailableMemoryBytes == 0 {
		c.MinAvailableMemoryBytes = 1 << 30
	}
	return c
}

func (c CodexCapacityRetryConfig) Budget() time.Duration {
	d, _ := time.ParseDuration(c.WithDefaults().TotalBudget)
	return d
}
func (c CodexCapacityRetryConfig) Validate() error {
	c = c.WithDefaults()
	budget, err := time.ParseDuration(c.TotalBudget)
	if err != nil || budget <= 0 || budget > 30*time.Minute || c.MaxAttempts < 1 || c.MaxAttempts > 8 || c.MaxPending < 1 || c.MaxPending > 16 || c.MaxPendingPerAccount < 1 || c.MaxPendingPerAccount > 8 || c.MaxPendingBytes < 1 || c.MaxPendingBytes > 128<<20 || c.MinAvailableMemoryBytes < 1<<30 {
		return fmt.Errorf("upstream.codex.capacity-retry: require budget in (0,30m], attempts 1..8, pending 1..16, pending-per-account 1..8, pending-bytes 1..134217728 and minimum available memory >= 1073741824")
	}
	return nil
}
