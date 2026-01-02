# procman

A lightweight Go library for managing long-running detached processes that survive application restarts, with reattachable progress monitoring.

## Features

- **Detached Execution**: Processes run independently via `setsid`, surviving parent termination
- **State Persistence**: Process state stored on disk for recovery after restarts
- **Reattachable Streams**: Tail stdout/stderr from any process, even after restart
- **Progress Callbacks**: Parse structured progress from process output line-by-line
- **Graceful Lifecycle**: Start, stop, cancel, and clean up processes
- **Label Filtering**: Organize and query jobs using custom metadata

## Installation

```bash
go get github.com/stevemurr/procman
```

## Quick Start

```go
package main

import (
    "context"
    "fmt"
    "log"
    "time"

    "github.com/stevemurr/procman"
)

func main() {
    // Create manager
    mgr, err := procman.New(procman.Config{})
    if err != nil {
        log.Fatal(err)
    }
    defer mgr.Close()

    // Start a background process
    jobID, err := mgr.Start(context.Background(), procman.StartOptions{
        Command: "sh",
        Args:    []string{"-c", "for i in 1 2 3; do echo \"Step $i\"; sleep 1; done"},
        Labels:  map[string]string{"type": "example"},
    })
    if err != nil {
        log.Fatal(err)
    }

    fmt.Printf("Started job: %s\n", jobID)

    // Watch progress
    mgr.Watch(context.Background(), jobID, func(line string) error {
        fmt.Printf("Output: %s\n", line)
        return nil
    })

    // Check final status
    job, _ := mgr.Get(jobID)
    fmt.Printf("Final status: %s\n", job.Status)
}
```

## API

### Manager

```go
// Create a new manager
mgr, err := procman.New(procman.Config{
    StateDir:     "~/.cache/myapp",  // Default: ~/.cache/procman
    CleanupAge:   24 * time.Hour,    // How long to keep completed jobs
    PollInterval: time.Second,       // Status check interval
})

// Start a detached process
jobID, err := mgr.Start(ctx, procman.StartOptions{
    Command: "my-command",
    Args:    []string{"arg1", "arg2"},
    Env:     []string{"KEY=value"},
    WorkDir: "/path/to/dir",
    Labels:  map[string]string{"key": "value"},
})

// Get job by ID
job, err := mgr.Get(jobID)

// List jobs with filtering
jobs, err := mgr.List(procman.ListOptions{
    Status: []procman.Status{procman.StatusRunning},
    Labels: map[string]string{"type": "download"},
})

// Attach to job output
reader, err := mgr.Attach(ctx, jobID, procman.AttachOptions{
    Stdout: true,
    Stderr: true,
    Follow: true,  // Like tail -f
})

// Watch with line-by-line callback
err := mgr.Watch(ctx, jobID, func(line string) error {
    fmt.Println(line)
    return nil  // Return error to stop watching
})

// Cancel a running job (SIGTERM, then SIGKILL after timeout)
err := mgr.Cancel(jobID, 5*time.Second)

// Remove job state and logs (must not be running)
err := mgr.Remove(jobID)

// Clean up old completed jobs
err := mgr.Cleanup()

// Refresh status of all jobs (detect orphaned processes)
err := mgr.Refresh()
```

### Job Status

| Status | Description |
|--------|-------------|
| `pending` | Created but not started |
| `running` | Process is running |
| `completed` | Exited with code 0 |
| `failed` | Exited with non-zero code |
| `cancelled` | Killed by user |
| `orphaned` | Process died unexpectedly |

## Reattaching After Restart

Processes survive application restarts. On startup, call `Refresh()` to update job statuses:

```go
mgr, _ := procman.New(procman.Config{})

// Detect orphaned processes
mgr.Refresh()

// Find running jobs
jobs, _ := mgr.List(procman.ListOptions{
    Status: []procman.Status{procman.StatusRunning},
})

for _, job := range jobs {
    fmt.Printf("Found running job: %s (PID %d)\n", job.ID, job.PID)

    // Reattach to monitor progress
    go mgr.Watch(context.Background(), job.ID, func(line string) error {
        fmt.Printf("[%s] %s\n", job.ID, line)
        return nil
    })
}
```

## File Structure

State is persisted to disk:

```
~/.cache/procman/
└── jobs/
    ├── abc123/
    │   ├── state.json     # Job metadata and status
    │   ├── stdout.log     # Stdout output
    │   └── stderr.log     # Stderr output
    └── def456/
        ├── state.json
        ├── stdout.log
        └── stderr.log
```

## Platform Support

Works on Linux and macOS (Unix systems with `setsid` support).

## License

MIT
