package config

import (
	"fmt"
	"os"
	"strconv"
	"time"
)

type FailoverConfig struct {
	Enabled         bool
	Region          string
	Grace, Interval time.Duration
	MaxAttempts     int
}

func LoadFailover(heartbeatTimeout time.Duration) (FailoverConfig, error) {
	var c FailoverConfig
	var err error
	c.Enabled, err = strconv.ParseBool(envOrDefault("LAUNCHPAD_WORKER_RECOVERY_ENABLED", "false"))
	if err != nil {
		return c, fmt.Errorf("invalid LAUNCHPAD_WORKER_RECOVERY_ENABLED")
	}
	c.Region = os.Getenv("AWS_REGION")
	if c.Enabled && c.Region == "" {
		return c, fmt.Errorf("AWS_REGION is required for EC2 worker fencing")
	}
	c.Grace, err = durationEnv("LAUNCHPAD_WORKER_RECOVERY_GRACE", "180s")
	if err != nil {
		return c, err
	}
	if c.Enabled && c.Grace <= heartbeatTimeout {
		return c, fmt.Errorf("worker recovery grace must exceed heartbeat timeout")
	}
	c.Interval, err = durationEnv("LAUNCHPAD_WORKER_RECOVERY_INTERVAL", "10s")
	if err != nil {
		return c, err
	}
	c.MaxAttempts, err = strconv.Atoi(envOrDefault("LAUNCHPAD_WORKER_RECOVERY_MAX_ATTEMPTS", "3"))
	if err != nil || c.MaxAttempts < 1 || c.MaxAttempts > 10 {
		return c, fmt.Errorf("worker recovery max attempts must be between 1 and 10")
	}
	return c, nil
}
