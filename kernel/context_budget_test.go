package kernel

import (
	"strings"
	"testing"
)

func TestPromptBudget_AddWithinLimit(t *testing.T) {
	b := NewPromptBudget(WithLimit(SectionMarketData, 100))
	got := b.Add(SectionMarketData, "hello world")
	if got != "hello world" {
		t.Fatalf("within-limit add should pass through, got %q", got)
	}
	if b.Used(SectionMarketData) != len("hello world") {
		t.Fatalf("used not tracked: %d", b.Used(SectionMarketData))
	}
	if b.HasOverflow() {
		t.Fatalf("expected no overflow")
	}
}

func TestPromptBudget_TrimsOnOverflow(t *testing.T) {
	b := NewPromptBudget(WithLimit(SectionMarketData, 20))
	got := b.Add(SectionMarketData, "0123456789012345678901234567890") // 31 chars
	if !strings.Contains(got, "[...truncated") {
		t.Fatalf("expected truncation marker; got %q", got)
	}
	// Trimmed content should be exactly the first 20 chars + the marker.
	if !strings.HasPrefix(got, "01234567890123456789") {
		t.Fatalf("truncated prefix wrong: %q", got)
	}
	if !b.HasOverflow() {
		t.Fatal("expected overflow flag")
	}
}

func TestPromptBudget_FurtherAddsDroppedAfterFull(t *testing.T) {
	b := NewPromptBudget(WithLimit(SectionMarketData, 10))
	first := b.Add(SectionMarketData, "0123456789ABCDE") // 15 → 10 used + 5 dropped
	if !strings.HasPrefix(first, "0123456789") {
		t.Fatalf("first add unexpected prefix: %q", first)
	}
	second := b.Add(SectionMarketData, "more content")
	if second != "" {
		t.Fatalf("section is full, second add should return empty; got %q", second)
	}
}

func TestPromptBudget_ReportShape(t *testing.T) {
	b := NewPromptBudget(WithLimit(SectionMarketData, 50))
	b.Add(SectionMarketData, strings.Repeat("x", 60)) // overflow by 10
	b.Add(SectionAccountState, "small")
	got := b.Report()
	for _, want := range []string{"market_data=", "account_state=", "(+10 dropped)", "total="} {
		if !strings.Contains(got, want) {
			t.Errorf("expected report to contain %q; got: %s", want, got)
		}
	}
}

func TestPromptBudget_NilSafe(t *testing.T) {
	var b *PromptBudget
	if got := b.Add(SectionMarketData, "anything"); got != "anything" {
		t.Fatalf("nil budget should pass through; got %q", got)
	}
	if b.HasOverflow() {
		t.Fatal("nil budget should not report overflow")
	}
	if b.Report() != "" {
		t.Fatal("nil budget should not produce a report")
	}
}
