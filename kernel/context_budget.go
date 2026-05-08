// Package kernel — context_budget.go
//
// Centralised token budget for AI prompt construction. Built because the
// trader cycle's prompt size grew organically — each indicator toggle, each
// reasoning trail, each market data section adds bytes without anyone
// asking "is the prompt still under the model's effective attention
// budget". Result: bigger prompts, slower responses, more attention
// dilution, no observability.
//
// This file makes the implicit explicit: each section declares its budget,
// the budget tracker enforces it during string assembly, and the trim
// policy is the same everywhere. Future additions (new indicators, new
// data sources) should claim a budget slice rather than splatting raw
// strings into prompt builders.
//
// Why per-section budgets and not just "trim the whole prompt to N":
//   - Different sections have different value densities. Recent reasoning
//     is high-value but compressible; market data is low-value-per-token
//     but uncompressible (numbers don't shrink without losing precision).
//   - Trimming uniformly drops the parts you most needed.
//   - Per-section caps let us fail loudly: if market data overflows by
//     30%, the user should know we're cutting their indicators, not just
//     silently shipping a smaller prompt.
//
// Budget units: characters (not tokens). Conversion factor ~3.5 chars/token
// for English+JSON, lower for CJK. Defaults aim for ~12k tokens total
// with headroom for the model's reasoning.
package kernel

import (
	"fmt"
	"strings"
)

// PromptSection is a logical block within a prompt. Order in the slice
// determines render order in the final string.
type PromptSection string

const (
	SectionRoleDefinition  PromptSection = "role_definition"
	SectionTradingFreq     PromptSection = "trading_frequency"
	SectionDecisionRules   PromptSection = "decision_rules"
	SectionDataDictionary  PromptSection = "data_dictionary"
	SectionAccountState    PromptSection = "account_state"
	SectionPositions       PromptSection = "positions"
	SectionMarketData      PromptSection = "market_data"
	SectionPastReasonings  PromptSection = "past_reasonings"
	SectionExtras          PromptSection = "extras"
)

// budgetCharsBySection caps each section's contribution. Tuned for a
// ~50KB total budget (~12k tokens) which fits comfortably in any modern
// model's effective attention window without crowding the response budget.
//
// Numbers are deliberate, not arbitrary:
//   - market_data: 60% — the AI's job is interpreting numbers. Cutting
//     this hurts the most.
//   - past_reasonings: 12% — a few cycles of context is plenty; older
//     reasonings reference stale prices anyway.
//   - role + decision_rules: 10% combined — these are stable strings.
//   - account + positions: 6% — usually small, cap is a safety net.
//   - data_dictionary: 8% — explains schema; can shrink in shipping.
//   - extras: 4% — for new sections without a permanent slot yet.
var budgetCharsBySection = map[PromptSection]int{
	SectionRoleDefinition: 3000,
	SectionTradingFreq:    1500,
	SectionDecisionRules:  3500,
	SectionDataDictionary: 4000,
	SectionAccountState:   1500,
	SectionPositions:      1500,
	SectionMarketData:     30000,
	SectionPastReasonings: 6000,
	SectionExtras:         2000,
}

// PromptBudget tracks how much each section consumed. Pass one through
// builders so they can call Add() with a section + content; the budget
// trims and emits a marker on overflow.
type PromptBudget struct {
	used    map[PromptSection]int
	limits  map[PromptSection]int
	overage map[PromptSection]int // bytes that were dropped due to overflow
}

// NewPromptBudget returns a fresh budget with the default per-section caps.
// Pass overrides via opts to tune for specific use cases (e.g. the
// optimizer prompt has different priorities than the trader cycle).
func NewPromptBudget(opts ...PromptBudgetOption) *PromptBudget {
	b := &PromptBudget{
		used:    make(map[PromptSection]int, len(budgetCharsBySection)),
		limits:  make(map[PromptSection]int, len(budgetCharsBySection)),
		overage: make(map[PromptSection]int),
	}
	for k, v := range budgetCharsBySection {
		b.limits[k] = v
	}
	for _, opt := range opts {
		opt(b)
	}
	return b
}

// PromptBudgetOption tweaks a PromptBudget at construction time.
type PromptBudgetOption func(*PromptBudget)

// WithLimit overrides a single section's cap.
func WithLimit(section PromptSection, chars int) PromptBudgetOption {
	return func(b *PromptBudget) {
		if chars > 0 {
			b.limits[section] = chars
		}
	}
}

// Add appends content under a section, trimming on overflow and recording
// dropped bytes. Returns the (possibly trimmed) content actually consumed.
//
// Trim policy: cut the tail. Tails are the lowest-priority part of every
// section we use today (market data is sorted by relevance descending;
// reasonings are most-recent first; positions are sorted by notional
// descending). If a section needs different trim semantics (e.g. cut the
// middle), it should pre-trim before calling Add.
func (b *PromptBudget) Add(section PromptSection, content string) string {
	if b == nil {
		return content
	}
	limit := b.limits[section]
	if limit <= 0 {
		// No cap configured = unbounded; track usage but don't trim.
		b.used[section] += len(content)
		return content
	}
	available := limit - b.used[section]
	if available <= 0 {
		b.overage[section] += len(content)
		return ""
	}
	if len(content) <= available {
		b.used[section] += len(content)
		return content
	}
	// Trim, mark with truncation tag so the model knows.
	trimmed := content[:available]
	dropped := len(content) - available
	b.used[section] = limit
	b.overage[section] += dropped
	return trimmed + fmt.Sprintf("\n[...truncated %d chars to fit budget...]\n", dropped)
}

// Used reports current consumption per section.
func (b *PromptBudget) Used(section PromptSection) int {
	if b == nil {
		return 0
	}
	return b.used[section]
}

// Total returns the sum of bytes consumed across all sections.
func (b *PromptBudget) Total() int {
	if b == nil {
		return 0
	}
	total := 0
	for _, v := range b.used {
		total += v
	}
	return total
}

// Report returns a human-readable summary of section usage vs limits.
// Logged once per cycle so we can spot creeping prompt bloat.
func (b *PromptBudget) Report() string {
	if b == nil {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("[prompt budget] ")
	first := true
	for _, sec := range []PromptSection{
		SectionRoleDefinition, SectionTradingFreq, SectionDecisionRules,
		SectionDataDictionary, SectionAccountState, SectionPositions,
		SectionMarketData, SectionPastReasonings, SectionExtras,
	} {
		if b.used[sec] == 0 && b.overage[sec] == 0 {
			continue
		}
		if !first {
			sb.WriteString(" ")
		}
		first = false
		flag := ""
		if b.overage[sec] > 0 {
			flag = "!"
		}
		sb.WriteString(fmt.Sprintf("%s=%d/%d%s",
			string(sec), b.used[sec], b.limits[sec], flag))
		if b.overage[sec] > 0 {
			sb.WriteString(fmt.Sprintf("(+%d dropped)", b.overage[sec]))
		}
	}
	sb.WriteString(fmt.Sprintf(" total=%d", b.Total()))
	return sb.String()
}

// HasOverflow returns true if any section was trimmed.
func (b *PromptBudget) HasOverflow() bool {
	if b == nil {
		return false
	}
	for _, v := range b.overage {
		if v > 0 {
			return true
		}
	}
	return false
}
