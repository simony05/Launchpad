package config

import (
	"fmt"
	"strconv"
	"time"
)

type RecoveryConfig struct {
	Interval    time.Duration
	Cooldown    time.Duration
	MaxRestarts int
}

func LoadRecovery() (RecoveryConfig, error) {
	cfg := RecoveryConfig{}
	var err error
	cfg.Interval, err = durationEnv("LAUNCHPAD_APP_HEALTH_INTERVAL", "10s")
	if err != nil {
		return cfg, err
	}
	cfg.Cooldown, err = durationEnv("LAUNCHPAD_RESTART_COOLDOWN", "30s")
	if err != nil {
		return cfg, err
	}
	cfg.MaxRestarts, err = strconv.Atoi(envOrDefault("LAUNCHPAD_MAX_RESTART_ATTEMPTS", "3"))
	if err != nil || cfg.MaxRestarts < 0 || cfg.MaxRestarts > 10 {
		return cfg, fmt.Errorf("LAUNCHPAD_MAX_RESTART_ATTEMPTS must be between 0 and 10")
	}
	return cfg, nil
}
