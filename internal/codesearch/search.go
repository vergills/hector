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
	Root    string
	Timeout time.Duration
}

func New(root string, timeout time.Duration) (*Searcher, error) {
	root, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("resolve repository path: %w", err)
	}
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	return &Searcher{Root: root, Timeout: timeout}, nil
}

func (s *Searcher) Search(query string) (string, error) {
	args, err := shellFields(query)
	if err != nil {
		return "", err
	}
	if len(args) == 0 || len(query) > 200 || strings.ContainsAny(query, "\r\n\x00") {
		return "", fmt.Errorf("invalid search query")
	}
	ctx, cancel := context.WithTimeout(context.Background(), s.Timeout)
	defer cancel()
	args = append([]string{"--exclude=.env"}, args...)
	args = append(args, ".")
	cmd := exec.CommandContext(ctx, "grep", args...)
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
		if message := strings.TrimSpace(string(output)); message != "" {
			return "", fmt.Errorf("grep: %s", message)
		}
		return "", fmt.Errorf("grep: %w", err)
	}
	return strings.TrimSpace(string(output)), nil
}

func shellFields(input string) ([]string, error) {
	var fields []string
	var current strings.Builder
	var quote rune
	escaped := false
	for _, r := range input {
		if escaped {
			current.WriteRune(r)
			escaped = false
			continue
		}
		if r == '\\' && quote != '\'' {
			escaped = true
			continue
		}
		if quote != 0 {
			if r == quote {
				quote = 0
			} else {
				current.WriteRune(r)
			}
			continue
		}
		if r == '\'' || r == '"' {
			quote = r
		} else if r == ' ' || r == '\t' {
			if current.Len() > 0 {
				fields = append(fields, current.String())
				current.Reset()
			}
		} else {
			current.WriteRune(r)
		}
	}
	if escaped || quote != 0 {
		return nil, fmt.Errorf("unterminated quote or escape")
	}
	if current.Len() > 0 {
		fields = append(fields, current.String())
	}
	return fields, nil
}
