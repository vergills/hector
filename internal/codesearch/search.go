package codesearch

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

var ErrNoMatches = errors.New("no code matches")

type Searcher struct {
	Root          string
	MaxOutputSize int
	Timeout       time.Duration
}

func New(root string, maxOutputSize int, timeout time.Duration) (*Searcher, error) {
	root, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("resolve repository path: %w", err)
	}
	if maxOutputSize <= 0 {
		maxOutputSize = 50000
	}
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	return &Searcher{Root: root, MaxOutputSize: maxOutputSize, Timeout: timeout}, nil
}

func (s *Searcher) Search(query string) (string, error) {
	if strings.TrimSpace(query) == "" || len(query) > 200 || strings.ContainsAny(query, "\r\n\x00") {
		return "", fmt.Errorf("invalid search query")
	}
	ctx, cancel := context.WithTimeout(context.Background(), s.Timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "grep", "-RInF", "-C", "3",
		"--exclude-dir=.git", "--exclude-dir=vendor", "--exclude-dir=node_modules",
		"--exclude=*.sum", "--exclude=.env", "--exclude=.env.*", "--", query, ".")
	cmd.Dir = s.Root
	output, err := cmd.CombinedOutput()
	if err != nil {
		if ctx.Err() != nil {
			return "", fmt.Errorf("search timed out")
		}
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
			return "", ErrNoMatches
		}
		return "", fmt.Errorf("grep: %w", err)
	}
	if len(output) > s.MaxOutputSize {
		output = output[:s.MaxOutputSize]
	}
	return strings.TrimSpace(string(output)), nil
}
