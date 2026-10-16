package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/hivepaas/function-runtimes/hivepaas/serve"
)

func env(vars map[string]string) func(string) string {
	return func(name string) string { return vars[name] }
}

func cgroupWith(t *testing.T, file, value string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, file)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(value+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

// WEB_CONCURRENCY is how many serve, all started; otherwise one, and more as
// calls need them, up to the concurrency and one per 32 MiB of the memory
// limit.
func TestHowManyProcessesServe(t *testing.T) {
	cfg := serve.Config{MaxConcurrency: 16}
	for name, tc := range map[string]struct {
		vars   map[string]string
		cgroup string
		want   size
	}{
		"WEB_CONCURRENCY":                      {map[string]string{"WEB_CONCURRENCY": "3"}, t.TempDir(), size{3, 3}},
		"WEB_CONCURRENCY over the concurrency": {map[string]string{"WEB_CONCURRENCY": "40"}, t.TempDir(), size{16, 16}},
		"no memory limit":                      {nil, t.TempDir(), size{1, 16}},
		"no memory limit in cgroup v2":         {nil, cgroupWith(t, "memory.max", "max"), size{1, 16}},
		"256 MiB":                              {nil, cgroupWith(t, "memory.max", "268435456"), size{1, 8}},
		"16 MiB":                               {nil, cgroupWith(t, "memory.max", "16777216"), size{1, 1}},
		"cgroup v1":                            {nil, cgroupWith(t, "memory/memory.limit_in_bytes", "134217728"), size{1, 4}},
		"cgroup v1 without a limit": {nil, cgroupWith(t, "memory/memory.limit_in_bytes", "9223372036854771712"),
			size{1, 16}},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := sizeFor(env(tc.vars), cfg, tc.cgroup)
			if err != nil || got != tc.want {
				t.Errorf("%v %v, want %v", got, err, tc.want)
			}
		})
	}
	if _, err := sizeFor(env(map[string]string{"WEB_CONCURRENCY": "0"}), cfg, t.TempDir()); err == nil {
		t.Error("WEB_CONCURRENCY=0 was taken")
	}
}

func TestTheServingLineSaysHowMany(t *testing.T) {
	for s, want := range map[size]string{
		{3, 3}:  "3 processes",
		{1, 1}:  "1 process",
		{1, 16}: "1 to 16 processes, as many as the calls need",
	} {
		if got := s.String(); got != want {
			t.Errorf("%v: %q, want %q", s, got, want)
		}
	}
}

// A line the function printed is passed on; the sync line after it is not,
// and says which call it ends - after a line the function did not finish too.
func TestSyncLinesAreTakenOutOfTheOutput(t *testing.T) {
	var passed []string
	synced := make(chan uint64, 4)
	pass := func(line string) { passed = append(passed, line) }

	passLine([]byte("hello\n"), pass, synced)
	passLine([]byte(outputSync+"7\n"), pass, synced)
	passLine([]byte("unfinished"+outputSync+"8\n"), pass, synced)
	passLine([]byte(outputSync+"not a number\n"), pass, synced)

	if want := []string{"hello", "unfinished", outputSync + "not a number"}; !slices.Equal(passed, want) {
		t.Errorf("passed %q, want %q", passed, want)
	}
	if got := []uint64{<-synced, <-synced}; !slices.Equal(got, []uint64{7, 8}) {
		t.Errorf("synced %v", got)
	}
}

// The function's php.ini is read after the runtime's settings, unless the
// function chose where PHP reads them.
func TestTheFunctionsPHPINI(t *testing.T) {
	dir := t.TempDir()
	base := []string{"A=1"}
	got, err := withFunctionINI(base, env(nil), dir)
	if err != nil || !slices.Equal(got, base) {
		t.Errorf("without one: %v %v", got, err)
	}

	if err := os.WriteFile(filepath.Join(dir, "php.ini"), []byte("memory_limit = 256M\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err = withFunctionINI(base, env(nil), dir)
	if err != nil || len(got) != 2 || !strings.HasPrefix(got[1], "PHP_INI_SCAN_DIR=:") {
		t.Fatalf("with one: %v %v", got, err)
	}
	copied, err := os.ReadFile(filepath.Join(strings.TrimPrefix(got[1], "PHP_INI_SCAN_DIR=:"), "function.ini"))
	if err != nil || string(copied) != "memory_limit = 256M\n" {
		t.Errorf("copied %q %v", copied, err)
	}

	got, _ = withFunctionINI(base, env(map[string]string{"PHP_INI_SCAN_DIR": "/mine"}), dir)
	if !slices.Equal(got, base) {
		t.Errorf("with PHP_INI_SCAN_DIR set: %v", got)
	}
}
