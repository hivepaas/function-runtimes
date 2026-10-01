package serve

import (
	"errors"
	"fmt"
	"strconv"
	"time"
)

// The contract's defaults.
const (
	defaultPort           = 8080
	defaultTimeoutMs      = 30000
	defaultMaxConcurrency = 16
	defaultMaxBodySize    = 6 << 20
)

var errNotPositive = errors.New("not a positive whole number")

// Config is the runtime's limits, read from the environment.
type Config struct {
	// Port is where serve listens (HP_FN_PORT).
	Port int
	// Timeout is how long one call may take (HP_FN_TIMEOUT_MS).
	Timeout time.Duration
	// MaxConcurrency is how many calls run at once (HP_FN_MAX_CONCURRENCY).
	MaxConcurrency int
	// MaxBodySize bounds a request's body and a response's (HP_FN_MAX_BODY_SIZE).
	MaxBodySize int64
}

// LoadConfig reads the configuration with getenv, the contract's defaults
// filling what is not set.
func LoadConfig(getenv func(string) string) (Config, error) {
	port, err := positive(getenv, "HP_FN_PORT", defaultPort)
	if err != nil {
		return Config{}, err
	}
	timeoutMs, err := positive(getenv, "HP_FN_TIMEOUT_MS", defaultTimeoutMs)
	if err != nil {
		return Config{}, err
	}
	concurrency, err := positive(getenv, "HP_FN_MAX_CONCURRENCY", defaultMaxConcurrency)
	if err != nil {
		return Config{}, err
	}
	bodySize, err := positive(getenv, "HP_FN_MAX_BODY_SIZE", defaultMaxBodySize)
	if err != nil {
		return Config{}, err
	}
	return Config{
		Port:           int(port),
		Timeout:        time.Duration(timeoutMs) * time.Millisecond,
		MaxConcurrency: int(concurrency),
		MaxBodySize:    bodySize,
	}, nil
}

func positive(getenv func(string) string, name string, fallback int64) (int64, error) {
	value := getenv(name)
	if value == "" {
		return fallback, nil
	}
	n, err := strconv.ParseInt(value, 10, 32)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("%s is %q: %w", name, value, errNotPositive)
	}
	return n, nil
}
