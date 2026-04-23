package cookies

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// Firefox user.js preferences to suppress first-run dialogs.
// Telemetry is explicitly disabled (healthreport upload off, toolkit
// telemetry off) in addition to bypassing the data-submission notification
// so we do not silently opt users into sending usage data just because
// Moombox launched their browser for cookie extraction.
const firefoxUserJS = `user_pref("browser.aboutwelcome.enabled", false);
user_pref("browser.shell.checkDefaultBrowser", false);
user_pref("browser.startup.homepage_override.mstone", "ignore");
user_pref("datareporting.policy.dataSubmissionPolicyBypassNotification", true);
user_pref("datareporting.healthreport.uploadEnabled", false);
user_pref("toolkit.telemetry.enabled", false);
user_pref("toolkit.telemetry.reportingpolicy.firstRun", false);
user_pref("browser.rights.3.shown", true);
`

// Firefox lock files that prevent launch when a previous session was force-killed.
var firefoxLockFiles = []string{"parent.lock", ".parentlock"}

func (s *AutoCookieService) startFirefoxSetup(browser *DetectedBrowser, url string) error {
	cleanFirefoxLockFiles(s.profileDir)

	// Write user.js to suppress first-run dialogs
	if err := os.WriteFile(filepath.Join(s.profileDir, "user.js"), []byte(firefoxUserJS), 0o644); err != nil {
		return fmt.Errorf("write user.js: %w", err)
	}

	cmd := exec.Command(browser.Path, "--new-instance", "--profile", s.profileDir, url)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start firefox: %w", err)
	}

	s.mu.Lock()
	s.setupCmd = cmd
	s.setupProcess = cmd.Process
	s.mu.Unlock()

	// Monitor for exit
	go func() {
		defer func() {
			if r := recover(); r != nil {
				s.logger.Error("panic in browser wait goroutine", "panic", r)
			}
		}()
		cmd.Wait()
		s.mu.Lock()
		s.browserExited = true
		s.mu.Unlock()
	}()

	return nil
}

func (s *AutoCookieService) closeFirefoxGracefully() {
	s.mu.Lock()
	proc := s.setupProcess
	exited := s.browserExited
	s.mu.Unlock()

	if proc == nil || exited {
		time.Sleep(300 * time.Millisecond)
		return
	}

	s.logger.Debug("sending graceful close to Firefox")

	// Graceful close
	if isWindows() {
		exec.Command("taskkill", "/T", "/PID", fmt.Sprintf("%d", proc.Pid)).Run()
	} else {
		proc.Signal(os.Interrupt)
	}

	// Wait up to 8 seconds for clean exit
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		s.mu.Lock()
		exited = s.browserExited
		s.mu.Unlock()
		if exited {
			s.logger.Debug("Firefox exited cleanly")
			time.Sleep(300 * time.Millisecond)
			return
		}
		time.Sleep(200 * time.Millisecond)
	}

	// Force kill
	s.logger.Warn("Firefox did not exit gracefully, force killing")
	killProcessTree(proc)
	time.Sleep(500 * time.Millisecond)
}

func (s *AutoCookieService) refreshFirefox(ctx context.Context, browser *DetectedBrowser) (string, error) {
	tempScreenshot := filepath.Join(s.profileDir, "refresh-screenshot.png")
	defer os.Remove(tempScreenshot)

	platforms := s.refreshPlatforms()
	for i, platform := range platforms {
		url := platformRefreshURLs[platform]

		// Wait between launches so Firefox fully releases the profile
		if i > 0 {
			s.logger.Info("waiting 5s before next Firefox launch", "platform", platform)
			time.Sleep(5 * time.Second)
		}

		if ctx.Err() != nil {
			return "", ctx.Err()
		}

		// Clean lock files right before launch — Firefox leaves parent.lock on exit
		cleanFirefoxLockFiles(s.profileDir)

		s.logger.Info("launching Firefox for cookie refresh", "platform", platform, "url", url)
		cmd := exec.Command(browser.Path, "--new-instance", "--screenshot", tempScreenshot, "--profile", s.profileDir, url)
		s.mu.Lock()
		s.refreshCmd = cmd
		s.mu.Unlock()
		startTime := time.Now()
		if err := runWithTimeout(cmd, processTimeout, s.logger); err != nil {
			s.logger.Warn("firefox "+platform+" refresh failed", "err", err, "elapsed", time.Since(startTime).Round(time.Millisecond))
		} else {
			s.logger.Info("firefox "+platform+" refresh completed", "elapsed", time.Since(startTime).Round(time.Millisecond))
		}
	}

	return readFirefoxCookies(s.profileDir)
}

func readFirefoxCookies(profileDir string) (string, error) {
	dbPath := filepath.Join(profileDir, "cookies.sqlite")
	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		return "", fmt.Errorf("firefox cookies.sqlite not found")
	}

	// Retry loop for SQLite WAL lock contention (Firefox may not have fully released the lock)
	const maxRetries = 5
	const retryBackoff = 500 * time.Millisecond

	var lines []string
	var lastErr error

	for attempt := range maxRetries {
		if attempt > 0 {
			time.Sleep(retryBackoff)
		}

		lines, lastErr = queryFirefoxCookieDB(dbPath)
		if lastErr == nil {
			break
		}
	}

	if lastErr != nil {
		return "", fmt.Errorf("query cookies after %d attempts: %w", maxRetries, lastErr)
	}

	result := []string{
		"# Netscape HTTP Cookie File",
		"# Extracted by Moombox auto-cookie service",
		"",
	}
	result = append(result, lines...)

	return strings.Join(result, "\n") + "\n", nil
}

// queryFirefoxCookieDB opens the Firefox cookie database and reads all cookies.
//
// _busy_timeout=2000 hands SQLite the wait itself — WAL-mode locks from a
// mid-flush Firefox process return "database is locked" almost immediately
// under the default 0ms busy timeout, and the caller's 5×500ms retry loop
// doesn't help if each open returns the error before even waiting. 2s lets
// SQLite block on the lock until Firefox's fsync completes, which is the
// desired behavior for most real-world contention.
func queryFirefoxCookieDB(dbPath string) ([]string, error) {
	db, err := sql.Open("sqlite", dbPath+"?mode=ro&_busy_timeout=2000")
	if err != nil {
		return nil, fmt.Errorf("open cookies.sqlite: %w", err)
	}
	defer db.Close()

	rows, err := db.Query("SELECT name, value, host, path, expiry, isHttpOnly, isSecure FROM moz_cookies")
	if err != nil {
		return nil, fmt.Errorf("query cookies: %w", err)
	}
	defer rows.Close()

	var collected []extractedCookie
	for rows.Next() {
		var name, value, host, cookiePath string
		var expiry, isHttpOnly, isSecure int64
		if err := rows.Scan(&name, &value, &host, &cookiePath, &expiry, &isHttpOnly, &isSecure); err != nil {
			continue
		}

		collected = append(collected, extractedCookie{
			domain:   host,
			httpOnly: isHttpOnly != 0,
			path:     cookiePath,
			secure:   isSecure != 0,
			expiry:   expiry,
			name:     name,
			value:    value,
		})
	}

	return deduplicateAndFormat(collected), nil
}

func cleanFirefoxLockFiles(profileDir string) {
	for _, name := range firefoxLockFiles {
		os.Remove(filepath.Join(profileDir, name))
	}
}

func runWithTimeout(cmd *exec.Cmd, timeout time.Duration, logger interface {
	Debug(msg string, args ...any)
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
}) error {
	// Create a Job Object so all child processes (including reparented ones)
	// are killed when we close the job handle.
	job, jobErr := newProcessJob()
	if jobErr != nil {
		logger.Warn("failed to create job object", "err", jobErr)
	} else if job != nil {
		logger.Debug("created job object for process tracking")
	}
	defer func() {
		if job != nil {
			logger.Debug("closing job object (killing all tracked processes)")
			job.close()
		}
	}()

	if err := cmd.Start(); err != nil {
		return err
	}
	logger.Debug("process started", "pid", cmd.Process.Pid)

	// Assign immediately after start so children are tracked from the beginning
	if job != nil {
		if err := job.assign(cmd.Process); err != nil {
			logger.Warn("failed to assign process to job object", "pid", cmd.Process.Pid, "err", err)
		} else {
			logger.Debug("assigned process to job object", "pid", cmd.Process.Pid)
		}
	}

	done := make(chan error, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				logger.Warn("panic in firefox wait goroutine", "panic", r)
			}
		}()
		done <- cmd.Wait()
	}()

	select {
	case err := <-done:
		logger.Debug("process exited normally", "pid", cmd.Process.Pid, "err", err)
		return err
	case <-time.After(timeout):
		logger.Warn("process timed out, killing", "pid", cmd.Process.Pid, "timeout", timeout)
		// Closing the job handle kills all processes in the job.
		// Also try direct kill as a belt-and-suspenders approach.
		killProcessTree(cmd.Process)
		// Wait briefly for reap, but don't block forever if kill failed
		select {
		case <-done:
			logger.Debug("process reaped after kill", "pid", cmd.Process.Pid)
		case <-time.After(5 * time.Second):
			logger.Warn("process did not exit after kill, forcing", "pid", cmd.Process.Pid)
			if cmd.Process != nil {
				cmd.Process.Kill()
			}
		}
		return fmt.Errorf("process timed out after %s", timeout)
	}
}
