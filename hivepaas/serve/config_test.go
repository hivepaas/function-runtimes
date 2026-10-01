package serve

import (
	"testing"
	"time"
)

func env(values map[string]string) func(string) string {
	return func(name string) string { return values[name] }
}

func TestTheDefaultsAreTheContracts(t *testing.T) {
	cfg, err := LoadConfig(env(nil))

	if err != nil {
		t.Fatal(err)
	}
	want := Config{Port: 8080, Timeout: 30 * time.Second, MaxConcurrency: 16, MaxBodySize: 6 << 20}
	if cfg != want {
		t.Fatalf("config = %+v, want %+v", cfg, want)
	}
}

func TestTheEnvironmentSetsTheLimits(t *testing.T) {
	cfg, err := LoadConfig(env(map[string]string{
		"HP_FN_PORT": "9000", "HP_FN_TIMEOUT_MS": "1500",
		"HP_FN_MAX_CONCURRENCY": "2", "HP_FN_MAX_BODY_SIZE": "1024",
	}))

	if err != nil {
		t.Fatal(err)
	}
	want := Config{Port: 9000, Timeout: 1500 * time.Millisecond, MaxConcurrency: 2, MaxBodySize: 1024}
	if cfg != want {
		t.Fatalf("config = %+v, want %+v", cfg, want)
	}
}

// A limit that is not a positive whole number stops the runtime: a function
// that would run without its timeout is worse than one that does not start.
func TestALimitThatIsNotAPositiveNumberIsRefused(t *testing.T) {
	for _, value := range []string{"0", "-1", "abc", "1.5"} {
		if _, err := LoadConfig(env(map[string]string{"HP_FN_TIMEOUT_MS": value})); err == nil {
			t.Errorf("HP_FN_TIMEOUT_MS=%q was accepted", value)
		}
	}
}
