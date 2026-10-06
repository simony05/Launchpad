package config

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"time"

	"github.com/google/uuid"
)

type RegistryConfig struct {
	Address                string
	Timeout, CheckInterval time.Duration
}
type HeartbeatConfig struct {
	InstanceID                             string
	ControlPlaneURL, ID, Hostname, Address string
	Interval                               time.Duration
}

func durationEnv(key, fallback string) (time.Duration, error) {
	value := envOrDefault(key, fallback)
	d, err := time.ParseDuration(value)
	if err != nil || d < time.Second || d > time.Hour {
		return 0, fmt.Errorf("%s must be a duration between 1s and 1h", key)
	}
	return d, nil
}
func LoadRegistry() (RegistryConfig, error) {
	cfg := RegistryConfig{Address: envOrDefault("LAUNCHPAD_REGISTRY_ADDRESS", "0.0.0.0:8091")}
	if _, _, err := net.SplitHostPort(cfg.Address); err != nil {
		return cfg, fmt.Errorf("invalid LAUNCHPAD_REGISTRY_ADDRESS: %w", err)
	}
	var err error
	cfg.Timeout, err = durationEnv("LAUNCHPAD_HEARTBEAT_TIMEOUT", "45s")
	if err != nil {
		return cfg, err
	}
	cfg.CheckInterval, err = durationEnv("LAUNCHPAD_HEARTBEAT_CHECK_INTERVAL", "5s")
	if err != nil {
		return cfg, err
	}
	if cfg.CheckInterval > cfg.Timeout {
		return cfg, fmt.Errorf("heartbeat check interval must not exceed timeout")
	}
	return cfg, nil
}
func LoadHeartbeat() (HeartbeatConfig, error) {
	hostname, err := os.Hostname()
	if err != nil {
		return HeartbeatConfig{}, err
	}
	cfg := HeartbeatConfig{ControlPlaneURL: os.Getenv("LAUNCHPAD_CONTROL_PLANE_URL"), ID: os.Getenv("LAUNCHPAD_WORKER_ID"), Hostname: envOrDefault("LAUNCHPAD_WORKER_HOSTNAME", hostname), Address: os.Getenv("LAUNCHPAD_WORKER_ADVERTISE_URL")}
	cfg.InstanceID = os.Getenv("LAUNCHPAD_WORKER_INSTANCE_ID")
	id, err := uuid.Parse(cfg.ID)
	if err != nil || id == uuid.Nil {
		return cfg, fmt.Errorf("LAUNCHPAD_WORKER_ID must be a nonzero UUID")
	}
	cfg.ID = id.String()
	for key, value := range map[string]string{"LAUNCHPAD_CONTROL_PLANE_URL": cfg.ControlPlaneURL, "LAUNCHPAD_WORKER_ADVERTISE_URL": cfg.Address} {
		u, err := url.Parse(value)
		if err != nil || u.Scheme != "http" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
			return cfg, fmt.Errorf("%s must be an HTTP origin such as http://10.0.0.10:8091", key)
		}
	}
	cfg.Interval, err = durationEnv("LAUNCHPAD_HEARTBEAT_INTERVAL", "10s")
	return cfg, err
}
