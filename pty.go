package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"bytes"
	"sync"
	"time"

	"github.com/charmbracelet/x/term"
	"github.com/charmbracelet/x/xpty"
)


func executeCommand(config Config) (string, error) {
	var promptLine string
	if config.ShowPrompt {
		prompt := config.PromptFormat
		if prompt == "" {
			prompt = "$"
		}

		user := os.Getenv("USER")
		if user == "" {
			user = "user"
		}

		hostname, err := os.Hostname()
		if err != nil {
			hostname = "host"
		}

		wd, err := os.Getwd()
		if err != nil {
			wd = "~"
		} else {
			home := os.Getenv("HOME")
			if home != "" && (wd == home || len(wd) > len(home) && wd[:len(home)] == home && (wd[len(home)] == '/' || len(wd) == len(home))) {
				wd = "~" + wd[len(home):]
			}
		}

		prompt = replacePromptVars(prompt, user, hostname, wd)
		promptLine = fmt.Sprintf("%s %s\n", prompt, config.Execute)
	}

	ctx, cancel := context.WithTimeout(context.Background(), config.ExecuteTimeout)
	defer cancel()

	width, height, err := term.GetSize(os.Stdout.Fd())
	if err != nil {
		width = 80
		height = 24
	}

	pty, err := xpty.NewPty(width, height)
	if err != nil {
		return "", fmt.Errorf("could not execute: %w", err)
	}
	defer func() { _ = pty.Close() }()

	cmd := exec.CommandContext(ctx, "setsid", "bash", "-lc", config.Execute) //nolint: gosec
	env := os.Environ()
	
	// Prevent sudo from opening /dev/tty directly
	env = append(env, "SUDO_ASKPASS=/bin/false")

	hasTerm := false
	for i, v := range env {
		if len(v) >= 5 && v[:5] == "TERM=" {
			env[i] = "TERM=xterm-256color"
			hasTerm = true
			break
		}
	}

	if !hasTerm {
		env = append(env, "TERM=xterm-256color")
		env = append(env, "COLORTERM=truecolor")
	}

	cmd.Env = env
	if err := pty.Start(cmd); err != nil {
		return "", fmt.Errorf("could not execute: %w", err)
	}

	// Set terminal to raw mode for proper PTY interaction
	oldState, err := term.MakeRaw(os.Stdin.Fd())
	if err == nil {
		defer func() { _ = term.Restore(os.Stdin.Fd(), oldState) }()
	}

	var out bytes.Buffer
	var errorOut bytes.Buffer
	var wg sync.WaitGroup
	wg.Add(1)
	
	// Copy stdin to pty for input (don't track in WaitGroup - it may never finish)
	go func() {
		_, _ = io.Copy(pty, os.Stdin)
	}()
	
	// Copy pty output to both stdout (for display) and buffer (for capture)
	go func() {
		defer wg.Done()
		multiWriter := io.MultiWriter(os.Stdout, &out)
		_, _ = io.Copy(multiWriter, pty)
		errorOut.Write(out.Bytes())
	}()

	processErr := xpty.WaitProcess(ctx, cmd)
	_ = pty.Close() // Close PTY to allow io.Copy to finish
	
	// Wait for output goroutine with a short grace period
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	
	// Give it 1 second to finish reading remaining output
	timer := context.Background()
	timerCtx, timerCancel := context.WithTimeout(timer, 1*time.Second)
	defer timerCancel()
	
	select {
	case <-done:
		// Output goroutine finished
	case <-timerCtx.Done():
		// Took too long, continue with what we have
	}
	
	if processErr != nil {
		// If ExpectTimeout is true and the error is a timeout, don't return an error
		if config.ExpectTimeout && ctx.Err() == context.DeadlineExceeded {
			cleaned := cleanCommandOutput(out.String())
			result := promptLine + cleaned
			return strings.TrimRight(result, "\n\r"), nil
		}
		return errorOut.String(), fmt.Errorf("could not execute: %w", processErr)
	}
	cleaned := cleanCommandOutput(out.String())
	result := promptLine + cleaned
	// Remove any trailing newlines from the result
	return strings.TrimRight(result, "\n\r"), nil
}


// cleanCommandOutput removes leading 'bash: ' error lines and trims trailing empty lines.
func cleanCommandOutput(s string) string {
	lines := strings.Split(s, "\n")
	// Remove leading 'bash: ' error lines
	start := 0
	for start < len(lines) && isBashErrorLine(lines[start]) {
		start++
	}
	// Remove trailing empty lines
	end := len(lines)
	for end > start && strings.TrimSpace(lines[end-1]) == "" {
		end--
	}
	if start >= end {
		return ""
	}
	return strings.Join(lines[start:end], "\n")
}

func isBashErrorLine(line string) bool {
	return len(line) >= 6 && line[:6] == "bash: "
}

// replacePromptVars replaces [user], [hostname], and [working directory] in the prompt string.
func replacePromptVars(prompt, user, hostname, wd string) string {
	p := prompt
	p = strings.ReplaceAll(p, "[user]", user)
	p = strings.ReplaceAll(p, "[hostname]", hostname)
	p = strings.ReplaceAll(p, "[wd]", wd)
	return p
}
