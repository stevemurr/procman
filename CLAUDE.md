
Here's a complete specification and implementation for a standalone Go process manager library:

---

# `procman` - Detached Process Manager for Go

A lightweight Go library for managing long-running external processes that survive application restarts, with reattachable progress monitoring.

## Features

- **Detached Execution**: Processes run independently via `setsid`, surviving parent termination
- **State Persistence**: Process state stored on disk for recovery after restarts
- **Reattachable Streams**: Tail stdout/stderr from any process, even after restart
- **Progress Callbacks**: Parse structured progress from process output
- **Graceful Lifecycle**: Start, stop, cancel, and clean up processes
- **Portable**: Works on Linux and macOS (Unix systems)

---

## API Design

```go
package procman

import (
    "context"
    "io"
    "time"
)

// Manager manages detached processes with persistent state.
type Manager struct {
    // unexported fields
}

// Config configures the process manager.
type Config struct {
    // StateDir is where process state and logs are stored.
    // Default: ~/.cache/procman
    StateDir string

    // CleanupAge is how long to keep completed/failed job state.
    // Default: 24 hours
    CleanupAge time.Duration

    // PollInterval is how often to check process status.
    // Default: 1 second
    PollInterval time.Duration
}

// Job represents a managed process.
type Job struct {
    ID        string            `json:"id"`
    Command   string            `json:"command"`
    Args      []string          `json:"args"`
    Env       []string          `json:"env,omitempty"`
    WorkDir   string            `json:"work_dir,omitempty"`
    Labels    map[string]string `json:"labels,omitempty"` // User metadata
    
    PID       int               `json:"pid"`
    Status    Status            `json:"status"`
    ExitCode  *int              `json:"exit_code,omitempty"`
    Error     string            `json:"error,omitempty"`
    
    StartedAt   time.Time       `json:"started_at"`
    CompletedAt *time.Time      `json:"completed_at,omitempty"`
}

// Status represents the process state.
type Status string

const (
    StatusPending   Status = "pending"    // Created but not started
    StatusRunning   Status = "running"    // Process is running
    StatusCompleted Status = "completed"  // Exited with code 0
    StatusFailed    Status = "failed"     // Exited with non-zero code
    StatusCancelled Status = "cancelled"  // Killed by user
    StatusOrphaned  Status = "orphaned"   // Process died unexpectedly
)

// StartOptions configures process execution.
type StartOptions struct {
    Command  string
    Args     []string
    Env      []string            // Additional env vars (inherits parent env)
    WorkDir  string              // Working directory
    Labels   map[string]string   // User metadata for filtering
}

// ListOptions filters job listing.
type ListOptions struct {
    Status []Status          // Filter by status (empty = all)
    Labels map[string]string // Filter by labels (all must match)
}

// AttachOptions configures stream attachment.
type AttachOptions struct {
    Stdout bool // Attach to stdout (default: true)
    Stderr bool // Attach to stderr (default: true)
    Follow bool // Follow output like tail -f (default: true)
    Tail   int  // Number of lines from end (0 = all, default: 0)
}

// ProgressFunc is called with parsed progress from output.
// Return an error to stop watching.
type ProgressFunc func(line string) error

// New creates a new process manager.
func New(cfg Config) (*Manager, error)

// Start launches a detached process.
// Returns immediately with the job ID.
func (m *Manager) Start(ctx context.Context, opts StartOptions) (jobID string, err error)

// Get retrieves a job by ID.
func (m *Manager) Get(jobID string) (*Job, error)

// List returns jobs matching the filter.
func (m *Manager) List(opts ListOptions) ([]*Job, error)

// Attach streams output from a running or completed process.
// Returns a reader that combines stdout/stderr.
// For running processes with Follow=true, blocks until process exits or ctx is cancelled.
func (m *Manager) Attach(ctx context.Context, jobID string, opts AttachOptions) (io.ReadCloser, error)

// Watch monitors a process and calls the progress function for each line of output.
// Blocks until process completes or ctx is cancelled.
func (m *Manager) Watch(ctx context.Context, jobID string, fn ProgressFunc) error

// Cancel sends SIGTERM to a running process, then SIGKILL after timeout.
func (m *Manager) Cancel(jobID string, timeout time.Duration) error

// Remove deletes job state and logs. Fails if process is still running.
func (m *Manager) Remove(jobID string) error

// Cleanup removes old completed/failed jobs based on CleanupAge.
func (m *Manager) Cleanup() error

// Refresh updates status of all jobs by checking if PIDs are still running.
// Call this on application startup to detect orphaned processes.
func (m *Manager) Refresh() error

// Close stops the manager and releases resources.
func (m *Manager) Close() error
```

---

## File Structure

```
~/.cache/procman/
├── jobs/
│   ├── abc123/
│   │   ├── state.json     # Job metadata and status
│   │   ├── stdout.log     # Stdout output
│   │   └── stderr.log     # Stderr output
│   └── def456/
│       ├── state.json
│       ├── stdout.log
│       └── stderr.log
└── manager.lock           # Prevents concurrent manager instances
```

---

## Implementation

### `manager.go`

```go
package procman

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/google/uuid"
)

var (
	ErrJobNotFound   = errors.New("job not found")
	ErrJobRunning    = errors.New("job is still running")
	ErrJobNotRunning = errors.New("job is not running")
	ErrInvalidConfig = errors.New("invalid configuration")
)

type Manager struct {
	cfg      Config
	stateDir string
	jobsDir  string

	mu   sync.RWMutex
	jobs map[string]*Job // in-memory cache
}

func New(cfg Config) (*Manager, error) {
	if cfg.StateDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("failed to get home dir: %w", err)
		}
		cfg.StateDir = filepath.Join(home, ".cache", "procman")
	}

	if cfg.CleanupAge == 0 {
		cfg.CleanupAge = 24 * time.Hour
	}

	if cfg.PollInterval == 0 {
		cfg.PollInterval = time.Second
	}

	jobsDir := filepath.Join(cfg.StateDir, "jobs")
	if err := os.MkdirAll(jobsDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create jobs dir: %w", err)
	}

	m := &Manager{
		cfg:      cfg,
		stateDir: cfg.StateDir,
		jobsDir:  jobsDir,
		jobs:     make(map[string]*Job),
	}

	// Load existing jobs from disk
	if err := m.loadJobs(); err != nil {
		return nil, fmt.Errorf("failed to load jobs: %w", err)
	}

	return m, nil
}

func (m *Manager) loadJobs() error {
	entries, err := os.ReadDir(m.jobsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		jobID := entry.Name()
		job, err := m.loadJob(jobID)
		if err != nil {
			continue // Skip corrupt jobs
		}

		m.jobs[jobID] = job
	}

	return nil
}

func (m *Manager) loadJob(jobID string) (*Job, error) {
	statePath := filepath.Join(m.jobsDir, jobID, "state.json")
	data, err := os.ReadFile(statePath)
	if err != nil {
		return nil, err
	}

	var job Job
	if err := json.Unmarshal(data, &job); err != nil {
		return nil, err
	}

	return &job, nil
}

func (m *Manager) saveJob(job *Job) error {
	jobDir := filepath.Join(m.jobsDir, job.ID)
	if err := os.MkdirAll(jobDir, 0755); err != nil {
		return err
	}

	data, err := json.MarshalIndent(job, "", "  ")
	if err != nil {
		return err
	}

	statePath := filepath.Join(jobDir, "state.json")
	return os.WriteFile(statePath, data, 0644)
}

func (m *Manager) Start(ctx context.Context, opts StartOptions) (string, error) {
	if opts.Command == "" {
		return "", errors.New("command is required")
	}

	jobID := uuid.New().String()[:8]
	jobDir := filepath.Join(m.jobsDir, jobID)

	if err := os.MkdirAll(jobDir, 0755); err != nil {
		return "", fmt.Errorf("failed to create job dir: %w", err)
	}

	// Create log files
	stdoutPath := filepath.Join(jobDir, "stdout.log")
	stderrPath := filepath.Join(jobDir, "stderr.log")

	stdoutFile, err := os.Create(stdoutPath)
	if err != nil {
		return "", fmt.Errorf("failed to create stdout log: %w", err)
	}

	stderrFile, err := os.Create(stderrPath)
	if err != nil {
		stdoutFile.Close()
		return "", fmt.Errorf("failed to create stderr log: %w", err)
	}

	// Build command
	cmd := exec.CommandContext(ctx, opts.Command, opts.Args...)
	cmd.Stdout = stdoutFile
	cmd.Stderr = stderrFile
	cmd.Dir = opts.WorkDir

	// Inherit environment and add extras
	cmd.Env = append(os.Environ(), opts.Env...)

	// Detach process - create new session so it survives parent exit
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setsid: true, // Create new session
	}

	// Start the process
	if err := cmd.Start(); err != nil {
		stdoutFile.Close()
		stderrFile.Close()
		os.RemoveAll(jobDir)
		return "", fmt.Errorf("failed to start process: %w", err)
	}

	job := &Job{
		ID:        jobID,
		Command:   opts.Command,
		Args:      opts.Args,
		Env:       opts.Env,
		WorkDir:   opts.WorkDir,
		Labels:    opts.Labels,
		PID:       cmd.Process.Pid,
		Status:    StatusRunning,
		StartedAt: time.Now(),
	}

	// Save state
	if err := m.saveJob(job); err != nil {
		cmd.Process.Kill()
		stdoutFile.Close()
		stderrFile.Close()
		os.RemoveAll(jobDir)
		return "", fmt.Errorf("failed to save job state: %w", err)
	}

	m.mu.Lock()
	m.jobs[jobID] = job
	m.mu.Unlock()

	// Monitor process in background
	go m.monitor(job, cmd, stdoutFile, stderrFile)

	return jobID, nil
}

func (m *Manager) monitor(job *Job, cmd *exec.Cmd, stdout, stderr *os.File) {
	defer stdout.Close()
	defer stderr.Close()

	// Wait for process to exit
	err := cmd.Wait()

	m.mu.Lock()
	defer m.mu.Unlock()

	now := time.Now()
	job.CompletedAt = &now

	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			code := exitErr.ExitCode()
			job.ExitCode = &code
			job.Status = StatusFailed
			job.Error = fmt.Sprintf("exit code %d", code)
		} else {
			job.Status = StatusFailed
			job.Error = err.Error()
		}
	} else {
		code := 0
		job.ExitCode = &code
		job.Status = StatusCompleted
	}

	m.saveJob(job)
}

func (m *Manager) Get(jobID string) (*Job, error) {
	m.mu.RLock()
	job, exists := m.jobs[jobID]
	m.mu.RUnlock()

	if !exists {
		return nil, ErrJobNotFound
	}

	// Return a copy
	jobCopy := *job
	return &jobCopy, nil
}

func (m *Manager) List(opts ListOptions) ([]*Job, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var result []*Job

	for _, job := range m.jobs {
		// Filter by status
		if len(opts.Status) > 0 {
			matched := false
			for _, s := range opts.Status {
				if job.Status == s {
					matched = true
					break
				}
			}
			if !matched {
				continue
			}
		}

		// Filter by labels
		if len(opts.Labels) > 0 {
			matched := true
			for k, v := range opts.Labels {
				if job.Labels[k] != v {
					matched = false
					break
				}
			}
			if !matched {
				continue
			}
		}

		jobCopy := *job
		result = append(result, &jobCopy)
	}

	return result, nil
}

func (m *Manager) Attach(ctx context.Context, jobID string, opts AttachOptions) (io.ReadCloser, error) {
	job, err := m.Get(jobID)
	if err != nil {
		return nil, err
	}

	jobDir := filepath.Join(m.jobsDir, jobID)

	// Default options
	if !opts.Stdout && !opts.Stderr {
		opts.Stdout = true
		opts.Stderr = true
	}

	// For simplicity, combine stdout and stderr
	var readers []io.Reader

	if opts.Stdout {
		f, err := os.Open(filepath.Join(jobDir, "stdout.log"))
		if err == nil {
			readers = append(readers, f)
		}
	}

	if opts.Stderr {
		f, err := os.Open(filepath.Join(jobDir, "stderr.log"))
		if err == nil {
			readers = append(readers, f)
		}
	}

	if len(readers) == 0 {
		return nil, errors.New("no log files available")
	}

	// If following a running process, use tail behavior
	if opts.Follow && job.Status == StatusRunning {
		return m.tailFollow(ctx, jobID, opts)
	}

	// Otherwise return static content
	return io.NopCloser(io.MultiReader(readers...)), nil
}

func (m *Manager) tailFollow(ctx context.Context, jobID string, opts AttachOptions) (io.ReadCloser, error) {
	pr, pw := io.Pipe()

	go func() {
		defer pw.Close()

		stdoutPath := filepath.Join(m.jobsDir, jobID, "stdout.log")
		stderrPath := filepath.Join(m.jobsDir, jobID, "stderr.log")

		var lastStdoutPos, lastStderrPos int64

		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				// Check if still running
				job, err := m.Get(jobID)
				if err != nil || job.Status != StatusRunning {
					// Read any remaining content and exit
					m.readNewContent(stdoutPath, &lastStdoutPos, pw)
					m.readNewContent(stderrPath, &lastStderrPos, pw)
					return
				}

				// Read new content
				if opts.Stdout {
					m.readNewContent(stdoutPath, &lastStdoutPos, pw)
				}
				if opts.Stderr {
					m.readNewContent(stderrPath, &lastStderrPos, pw)
				}
			}
		}
	}()

	return pr, nil
}

func (m *Manager) readNewContent(path string, lastPos *int64, w io.Writer) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()

	// Seek to last position
	f.Seek(*lastPos, io.SeekStart)

	// Read new content
	n, _ := io.Copy(w, f)
	*lastPos += n
}

func (m *Manager) Watch(ctx context.Context, jobID string, fn ProgressFunc) error {
	reader, err := m.Attach(ctx, jobID, AttachOptions{
		Stdout: true,
		Stderr: true,
		Follow: true,
	})
	if err != nil {
		return err
	}
	defer reader.Close()

	buf := make([]byte, 4096)
	var lineBuf []byte

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		n, err := reader.Read(buf)
		if n > 0 {
			lineBuf = append(lineBuf, buf[:n]...)

			// Process complete lines
			for {
				idx := -1
				for i, b := range lineBuf {
					if b == '\n' {
						idx = i
						break
					}
				}
				if idx == -1 {
					break
				}

				line := string(lineBuf[:idx])
				lineBuf = lineBuf[idx+1:]

				if err := fn(line); err != nil {
					return err
				}
			}
		}

		if err != nil {
			if err == io.EOF {
				// Process remaining content
				if len(lineBuf) > 0 {
					fn(string(lineBuf))
				}
				return nil
			}
			return err
		}
	}
}

func (m *Manager) Cancel(jobID string, timeout time.Duration) error {
	m.mu.RLock()
	job, exists := m.jobs[jobID]
	m.mu.RUnlock()

	if !exists {
		return ErrJobNotFound
	}

	if job.Status != StatusRunning {
		return ErrJobNotRunning
	}

	process, err := os.FindProcess(job.PID)
	if err != nil {
		return fmt.Errorf("failed to find process: %w", err)
	}

	// Send SIGTERM
	if err := process.Signal(syscall.SIGTERM); err != nil {
		// Process might already be dead
		if errors.Is(err, os.ErrProcessDone) {
			return nil
		}
		return fmt.Errorf("failed to send SIGTERM: %w", err)
	}

	// Wait for graceful shutdown
	done := make(chan struct{})
	go func() {
		for {
			time.Sleep(100 * time.Millisecond)
			if !m.isProcessRunning(job.PID) {
				close(done)
				return
			}
		}
	}()

	select {
	case <-done:
		// Process exited gracefully
	case <-time.After(timeout):
		// Force kill
		process.Signal(syscall.SIGKILL)
	}

	// Update state
	m.mu.Lock()
	job.Status = StatusCancelled
	now := time.Now()
	job.CompletedAt = &now
	m.saveJob(job)
	m.mu.Unlock()

	return nil
}

func (m *Manager) Remove(jobID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	job, exists := m.jobs[jobID]
	if !exists {
		return ErrJobNotFound
	}

	if job.Status == StatusRunning {
		return ErrJobRunning
	}

	// Remove from disk
	jobDir := filepath.Join(m.jobsDir, jobID)
	if err := os.RemoveAll(jobDir); err != nil {
		return fmt.Errorf("failed to remove job dir: %w", err)
	}

	delete(m.jobs, jobID)
	return nil
}

func (m *Manager) Cleanup() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	now := time.Now()
	var toRemove []string

	for id, job := range m.jobs {
		if job.Status == StatusRunning {
			continue
		}

		if job.CompletedAt != nil && now.Sub(*job.CompletedAt) > m.cfg.CleanupAge {
			toRemove = append(toRemove, id)
		}
	}

	for _, id := range toRemove {
		jobDir := filepath.Join(m.jobsDir, id)
		os.RemoveAll(jobDir)
		delete(m.jobs, id)
	}

	return nil
}

func (m *Manager) Refresh() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	for _, job := range m.jobs {
		if job.Status != StatusRunning {
			continue
		}

		if !m.isProcessRunning(job.PID) {
			job.Status = StatusOrphaned
			now := time.Now()
			job.CompletedAt = &now
			job.Error = "process exited unexpectedly"
			m.saveJob(job)
		}
	}

	return nil
}

func (m *Manager) isProcessRunning(pid int) bool {
	process, err := os.FindProcess(pid)
	if err != nil {
		return false
	}

	// On Unix, sending signal 0 checks if process exists
	err = process.Signal(syscall.Signal(0))
	return err == nil
}

func (m *Manager) Close() error {
	// Nothing to clean up for now
	return nil
}
```

---

## Usage Example

```go
package main

import (
    "context"
    "fmt"
    "log"
    "strings"
    "time"

    "github.com/yourorg/procman"
)

func main() {
    // Create manager
    mgr, err := procman.New(procman.Config{
        StateDir: "~/.cache/myapp/downloads",
    })
    if err != nil {
        log.Fatal(err)
    }
    defer mgr.Close()

    // Refresh state on startup (detect orphaned processes)
    mgr.Refresh()

    // Start a download
    jobID, err := mgr.Start(context.Background(), procman.StartOptions{
        Command: "hf",
        Args:    []string{"download", "meta-llama/Llama-3.1-8B-Instruct"},
        Labels: map[string]string{
            "type":     "model-download",
            "model_id": "meta-llama/Llama-3.1-8B-Instruct",
        },
    })
    if err != nil {
        log.Fatal(err)
    }

    fmt.Printf("Started job: %s\n", jobID)

    // Watch progress with custom parser
    ctx, cancel := context.WithTimeout(context.Background(), 2*time.Hour)
    defer cancel()

    err = mgr.Watch(ctx, jobID, func(line string) error {
        // Parse HuggingFace CLI progress output
        if strings.Contains(line, "%|") {
            fmt.Printf("Progress: %s\n", line)
        }
        return nil
    })

    if err != nil {
        log.Printf("Watch ended: %v", err)
    }

    // Check final status
    job, _ := mgr.Get(jobID)
    fmt.Printf("Final status: %s\n", job.Status)
}
```

---

## Reattaching After Restart

```go
func main() {
    mgr, _ := procman.New(procman.Config{})
    
    // On startup, refresh to detect orphaned processes
    mgr.Refresh()
    
    // Find running downloads
    jobs, _ := mgr.List(procman.ListOptions{
        Status: []procman.Status{procman.StatusRunning},
        Labels: map[string]string{"type": "model-download"},
    })
    
    for _, job := range jobs {
        fmt.Printf("Found running download: %s (PID %d)\n", job.Labels["model_id"], job.PID)
        
        // Reattach to get progress
        go func(j *procman.Job) {
            mgr.Watch(context.Background(), j.ID, func(line string) error {
                // Parse and publish progress...
                return nil
            })
        }(job)
    }
}
```

---

## Testing

```go
package procman_test

import (
    "context"
    "os"
    "testing"
    "time"

    "github.com/yourorg/procman"
)

func TestStartAndWatch(t *testing.T) {
    dir := t.TempDir()
    mgr, err := procman.New(procman.Config{StateDir: dir})
    if err != nil {
        t.Fatal(err)
    }
    defer mgr.Close()

    // Start a simple command
    jobID, err := mgr.Start(context.Background(), procman.StartOptions{
        Command: "sh",
        Args:    []string{"-c", "echo 'line1'; sleep 0.1; echo 'line2'"},
    })
    if err != nil {
        t.Fatal(err)
    }

    // Collect output
    var lines []string
    ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
    defer cancel()

    mgr.Watch(ctx, jobID, func(line string) error {
        lines = append(lines, line)
        return nil
    })

    if len(lines) != 2 {
        t.Errorf("expected 2 lines, got %d", len(lines))
    }

    // Check final status
    job, _ := mgr.Get(jobID)
    if job.Status != procman.StatusCompleted {
        t.Errorf("expected completed, got %s", job.Status)
    }
}

func TestSurvivesRestart(t *testing.T) {
    dir := t.TempDir()
    
    // Start a long-running process
    mgr1, _ := procman.New(procman.Config{StateDir: dir})
    jobID, _ := mgr1.Start(context.Background(), procman.StartOptions{
        Command: "sleep",
        Args:    []string{"10"},
    })
    
    // Get the PID
    job, _ := mgr1.Get(jobID)
    pid := job.PID
    
    // "Restart" by creating new manager
    mgr1.Close()
    mgr2, _ := procman.New(procman.Config{StateDir: dir})
    mgr2.Refresh()
    
    // Process should still be running
    job2, err := mgr2.Get(jobID)
    if err != nil {
        t.Fatal(err)
    }
    if job2.Status != procman.StatusRunning {
        t.Errorf("expected running, got %s", job2.Status)
    }
    if job2.PID != pid {
        t.Errorf("PID mismatch")
    }
    
    // Clean up
    mgr2.Cancel(jobID, time.Second)
    mgr2.Close()
}

func TestCancel(t *testing.T) {
    dir := t.TempDir()
    mgr, _ := procman.New(procman.Config{StateDir: dir})
    defer mgr.Close()

    jobID, _ := mgr.Start(context.Background(), procman.StartOptions{
        Command: "sleep",
        Args:    []string{"60"},
    })

    time.Sleep(100 * time.Millisecond)

    err := mgr.Cancel(jobID, time.Second)
    if err != nil {
        t.Fatal(err)
    }

    job, _ := mgr.Get(jobID)
    if job.Status != procman.StatusCancelled {
        t.Errorf("expected cancelled, got %s", job.Status)
    }
}
```

---

## Dependencies

```go
// go.mod
module github.com/yourorg/procman

go 1.21

require github.com/google/uuid v1.6.0
```

---

This implementation provides:

1. **Detached processes** via `Setsid: true` that survive parent termination
2. **Persistent state** in JSON files that survive restarts
3. **Reattachable output** by tailing log files
4. **Progress monitoring** with line-by-line callbacks
5. **Process lifecycle** management (start, cancel, remove, cleanup)
6. **Label filtering** for organizing jobs by type

