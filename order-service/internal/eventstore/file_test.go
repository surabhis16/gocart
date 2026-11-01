package eventstore

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFileStoreMeetsTheContract(t *testing.T) {
	runContract(t, func(t *testing.T) Store {
		s, err := OpenFile(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = s.Close() })
		return s
	})
}

func TestMemoryOnlyStoreMeetsTheContract(t *testing.T) {
	runContract(t, func(t *testing.T) Store {
		s, err := OpenFile("")
		if err != nil {
			t.Fatal(err)
		}
		return s
	})
}

func TestEventsSurviveRestart(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s, _ := OpenFile(dir)
	for v := 1; v <= 3; v++ {
		if err := s.Append(ctx, ev("ord_a", v)); err != nil {
			t.Fatal(err)
		}
	}
	_ = s.Append(ctx, ev("ord_b", 1))
	_ = s.Close()

	again, err := OpenFile(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	if got, _ := again.Load(ctx, "ord_a"); len(got) != 3 {
		t.Errorf("ord_a has %d events after restart, want 3", len(got))
	}
	if all, _ := again.LoadAll(ctx); len(all) != 4 {
		t.Errorf("%d events after restart, want 4", len(all))
	}
	if err := again.Append(ctx, ev("ord_a", 4)); err != nil {
		t.Errorf("cannot continue a stream after restart: %v", err)
	}
}

// A crash mid-write leaves half a line. That write was never acknowledged, so
// dropping it is right, and the log must be usable afterwards.
func TestTornFinalWriteIsRepaired(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s, _ := OpenFile(dir)
	_ = s.Append(ctx, ev("ord_a", 1))
	_ = s.Append(ctx, ev("ord_a", 2))
	_ = s.Close()

	path := filepath.Join(dir, "events.log")
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	torn := `{"stream_id":"ord_a","version":3,"type":"Ord`
	_, _ = f.WriteString(torn)
	_ = f.Close()

	again, err := OpenFile(dir)
	if err != nil {
		t.Fatalf("Open refused a log with only a torn tail: %v", err)
	}
	if again.RepairedBytes != int64(len(torn)) {
		t.Errorf("RepairedBytes = %d, want %d", again.RepairedBytes, len(torn))
	}
	if got, _ := again.Load(ctx, "ord_a"); len(got) != 2 {
		t.Errorf("%d events survived, want 2", len(got))
	}
	if err := again.Append(ctx, ev("ord_a", 3)); err != nil {
		t.Fatalf("append after repair: %v", err)
	}
	_ = again.Close()

	third, err := OpenFile(dir)
	if err != nil {
		t.Fatalf("log poisoned by the repair: %v", err)
	}
	defer third.Close()
	if got, _ := third.Load(ctx, "ord_a"); len(got) != 3 || third.RepairedBytes != 0 {
		t.Errorf("after reopen: %d events, repaired %d bytes", len(got), third.RepairedBytes)
	}
}

// Corruption that is not at the tail cannot be a crash, so the service must
// stop instead of quietly serving a history with a hole in it.
func TestCorruptionInTheMiddleIsRefused(t *testing.T) {
	dir := t.TempDir()
	s, _ := OpenFile(dir)
	ctx := context.Background()
	for v := 1; v <= 3; v++ {
		_ = s.Append(ctx, ev("ord_a", v))
	}
	_ = s.Close()

	path := filepath.Join(dir, "events.log")
	b, _ := os.ReadFile(path)
	lines := strings.SplitAfter(string(b), "\n")
	lines[1] = "this is not json\n"
	_ = os.WriteFile(path, []byte(strings.Join(lines, "")), 0o644)

	if _, err := OpenFile(dir); err == nil || !strings.Contains(err.Error(), "line 2") {
		t.Errorf("err = %v, want a refusal naming line 2", err)
	}
}

func TestVersionGapInTheLogIsRefused(t *testing.T) {
	dir := t.TempDir()
	good, _ := OpenFile(dir)
	_ = good.Append(context.Background(), ev("ord_a", 1))
	_ = good.Close()

	path := filepath.Join(dir, "events.log")
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	_, _ = f.WriteString(`{"stream_id":"ord_a","version":3,"type":"OrderShipped","data":{},"at":"2026-10-30T09:00:00Z"}` + "\n")
	_ = f.Close()

	if _, err := OpenFile(dir); err == nil {
		t.Error("Open accepted a log where version 2 is missing")
	}
}

func TestAppendAfterCloseFails(t *testing.T) {
	s, _ := OpenFile(t.TempDir())
	_ = s.Close()
	if err := s.Append(context.Background(), ev("ord_a", 1)); err == nil {
		t.Error("Append on a closed store succeeded; the event would be lost")
	}
}
