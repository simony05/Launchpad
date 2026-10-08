package config

import (
	"fmt"
	"strconv"
	"time"
)

type TTLConfig struct {
	DefaultSeconds int64
	CheckInterval  time.Duration
}

func LoadTTL() (TTLConfig, error) {
	var c TTLConfig
	var err error
	c.DefaultSeconds, err = strconv.ParseInt(envOrDefault("LAUNCHPAD_DEFAULT_TTL_SECONDS", "604800"), 10, 64)
	if err != nil || c.DefaultSeconds < 1 || c.DefaultSeconds > 31536000 {
		return c, fmt.Errorf("LAUNCHPAD_DEFAULT_TTL_SECONDS must be from 1 to 31536000")
	}
	c.CheckInterval, err = durationEnv("LAUNCHPAD_TTL_CHECK_INTERVAL", "15s")
	return c, err
}
