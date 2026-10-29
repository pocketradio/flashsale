package main

import (
	"testing"
	"time"
)

func TestDesiredScalesOnQueueAge(t *testing.T) {
	got := desired(sample{Current: 4, Healthy: 4, QueueAge: 3 * time.Second}, 4, 8, 50, 2*time.Second)
	if got != 6 {
		t.Fatalf("got %d", got)
	}
}
func TestDesiredRespectsBounds(t *testing.T) {
	if got := desired(sample{Current: 8, CPU: 99, QueueAge: 10 * time.Second}, 4, 8, 50, 2*time.Second); got != 8 {
		t.Fatalf("got %d", got)
	}
	if got := desired(sample{Current: 4, CPU: 1}, 4, 8, 50, 2*time.Second); got != 4 {
		t.Fatalf("got %d", got)
	}
}
