package main

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
)

// deps installs composer.json's packages into dir/vendor, from composer.lock
// when there is one, and writes composer.lock when there is none. What it
// downloads is kept nowhere: the image keeps the packages alone.
func deps(dir string, stdout, stderr io.Writer) int {
	if _, err := os.Stat(filepath.Join(dir, "composer.json")); errors.Is(err, fs.ErrNotExist) {
		return exitOK
	}
	cache, err := os.MkdirTemp("", "hivepaas-deps-cache-")
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "hivepaas: %v\n", err)
		return exitFailed
	}
	defer func() { _ = os.RemoveAll(cache) }()
	cmd := exec.Command("composer", "install", "--no-dev", "--no-interaction", "--no-progress", //nolint:gosec // fixed
		"--prefer-dist", "--optimize-autoloader")
	cmd.Dir = dir
	cmd.Stdout, cmd.Stderr = stdout, stderr
	cmd.Env = append(os.Environ(), "COMPOSER_CACHE_DIR="+cache)
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return exitErr.ExitCode()
		}
		_, _ = fmt.Fprintf(stderr, "hivepaas: %v\n", err)
		return exitFailed
	}
	return exitOK
}
