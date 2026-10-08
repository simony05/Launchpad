package config

import (
	"fmt"
	"strconv"
	"time"
)

type IdleConfig struct {
	Enabled                        bool
	Timeout, Interval, WakeTimeout time.Duration
}

func LoadIdle() (IdleConfig, error) {
	var c IdleConfig
	var err error
	c.Enabled, err = strconv.ParseBool(envOrDefault("LAUNCHPAD_SCALE_TO_ZERO_ENABLED", "false"))
	if err != nil {
		return c, fmt.Errorf("invalid LAUNCHPAD_SCALE_TO_ZERO_ENABLED")
	}
	c.Timeout, err = durationEnv("LAUNCHPAD_IDLE_TIMEOUT", "15m")
	if err != nil {
		return c, err
	}
	c.Interval, err = durationEnv("LAUNCHPAD_IDLE_CHECK_INTERVAL", "30s")
	if err != nil {
		return c, err
	}
	c.WakeTimeout, err = durationEnv("LAUNCHPAD_COLD_START_TIMEOUT", "60s")
	if err != nil {
		return c, err
	}
	if c.WakeTimeout > 2*time.Minute {
		return c, fmt.Errorf("cold start timeout must not exceed 2m")
	}
	return c, nil
}
