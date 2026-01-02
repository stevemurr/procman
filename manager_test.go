package procman_test

import (
	"context"
	"testing"
	"time"

	"github.com/stevemurr/procman"
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

func TestList(t *testing.T) {
	dir := t.TempDir()
	mgr, err := procman.New(procman.Config{StateDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.Close()

	// Start a job with labels
	jobID, err := mgr.Start(context.Background(), procman.StartOptions{
		Command: "sleep",
		Args:    []string{"60"},
		Labels: map[string]string{
			"type":   "test",
			"region": "us-west",
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	time.Sleep(100 * time.Millisecond)

	// List all jobs
	jobs, err := mgr.List(procman.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 {
		t.Errorf("expected 1 job, got %d", len(jobs))
	}

	// List by status
	jobs, err = mgr.List(procman.ListOptions{
		Status: []procman.Status{procman.StatusRunning},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 {
		t.Errorf("expected 1 running job, got %d", len(jobs))
	}

	// List by labels
	jobs, err = mgr.List(procman.ListOptions{
		Labels: map[string]string{"type": "test"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 {
		t.Errorf("expected 1 job with label type=test, got %d", len(jobs))
	}

	// List by non-matching labels
	jobs, err = mgr.List(procman.ListOptions{
		Labels: map[string]string{"type": "other"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 0 {
		t.Errorf("expected 0 jobs with label type=other, got %d", len(jobs))
	}

	// Clean up
	mgr.Cancel(jobID, time.Second)
}

func TestRemove(t *testing.T) {
	dir := t.TempDir()
	mgr, err := procman.New(procman.Config{StateDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.Close()

	// Start a quick command
	jobID, err := mgr.Start(context.Background(), procman.StartOptions{
		Command: "true",
	})
	if err != nil {
		t.Fatal(err)
	}

	// Wait for it to complete
	time.Sleep(200 * time.Millisecond)

	// Remove the job
	err = mgr.Remove(jobID)
	if err != nil {
		t.Fatal(err)
	}

	// Verify it's gone
	_, err = mgr.Get(jobID)
	if err != procman.ErrJobNotFound {
		t.Errorf("expected ErrJobNotFound, got %v", err)
	}
}

func TestRemoveRunningFails(t *testing.T) {
	dir := t.TempDir()
	mgr, err := procman.New(procman.Config{StateDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.Close()

	// Start a long-running command
	jobID, err := mgr.Start(context.Background(), procman.StartOptions{
		Command: "sleep",
		Args:    []string{"60"},
	})
	if err != nil {
		t.Fatal(err)
	}

	time.Sleep(100 * time.Millisecond)

	// Try to remove while running
	err = mgr.Remove(jobID)
	if err != procman.ErrJobRunning {
		t.Errorf("expected ErrJobRunning, got %v", err)
	}

	// Clean up
	mgr.Cancel(jobID, time.Second)
}

func TestFailedCommand(t *testing.T) {
	dir := t.TempDir()
	mgr, err := procman.New(procman.Config{StateDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.Close()

	// Start a command that will fail
	jobID, err := mgr.Start(context.Background(), procman.StartOptions{
		Command: "sh",
		Args:    []string{"-c", "exit 42"},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Wait for it to complete
	time.Sleep(200 * time.Millisecond)

	job, err := mgr.Get(jobID)
	if err != nil {
		t.Fatal(err)
	}

	if job.Status != procman.StatusFailed {
		t.Errorf("expected failed, got %s", job.Status)
	}

	if job.ExitCode == nil || *job.ExitCode != 42 {
		t.Errorf("expected exit code 42, got %v", job.ExitCode)
	}
}

func TestCleanup(t *testing.T) {
	dir := t.TempDir()
	mgr, err := procman.New(procman.Config{
		StateDir:   dir,
		CleanupAge: 100 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.Close()

	// Start a quick command
	jobID, err := mgr.Start(context.Background(), procman.StartOptions{
		Command: "true",
	})
	if err != nil {
		t.Fatal(err)
	}

	// Wait for completion
	time.Sleep(200 * time.Millisecond)

	// Job should exist
	_, err = mgr.Get(jobID)
	if err != nil {
		t.Fatal(err)
	}

	// Wait past cleanup age
	time.Sleep(150 * time.Millisecond)

	// Cleanup should remove it
	err = mgr.Cleanup()
	if err != nil {
		t.Fatal(err)
	}

	// Job should be gone
	_, err = mgr.Get(jobID)
	if err != procman.ErrJobNotFound {
		t.Errorf("expected ErrJobNotFound after cleanup, got %v", err)
	}
}
