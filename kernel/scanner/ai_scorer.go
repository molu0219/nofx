package scanner

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"nofx/logger"
	"nofx/mcp"
)

// AIScorer plugs Claude (or any other mcp.AIClient) into the Scanner Scorer
// interface. The model receives a compact one-line-per-symbol summary of the
// entire ~561-perp universe (10m/30m/1h/24h deltas, volume, funding) and
// returns the N most interesting symbols for the configured trading mandate.
//
// Fallback safety:
//   - On any AI error, parse failure, or fewer-than-half-expected symbols
//     returned, AIScorer falls back to its embedded rule Scorer so the
//     trader never starves for a watchlist.
//   - A short cache de-duplicates AI calls within the same scan window
//     (default 9 min, tuned slightly under the 10-min scan interval so a
//     refresh that fires a few seconds early still hits cache).
type AIScorer struct {
	// Client is the LLM that picks symbols. Required.
	Client mcp.AIClient
	// Top is how many symbols the prompt asks the model to pick. The
	// scanner watchlist downstream is usually smaller; we ask for a buffer
	// (e.g. Top=50 for a 30-watchlist) so hysteresis has cushion.
	Top int
	// Fallback ranks symbols when the AI call fails. Required (typically a
	// RuleScorer with default weights).
	Fallback Scorer
	// CacheTTL caps how often we spend an AI call. 0 → 9 minutes.
	CacheTTL time.Duration

	mu       sync.Mutex
	cached   []string
	cachedAt time.Time
}

// Name implements Scorer.
func (a *AIScorer) Name() string { return "ai" }

// Rank implements Scorer. Returns symbols in pick order, with rule-scorer
// suggestions appended for any universe symbols the AI didn't pick (so the
// downstream watchlist composer always has enough to populate hysteresis +
// position protection slots).
func (a *AIScorer) Rank(entries []UniverseEntry) []string {
	if a.Client == nil || a.Fallback == nil || len(entries) == 0 {
		if a.Fallback != nil {
			return a.Fallback.Rank(entries)
		}
		return nil
	}
	ttl := a.CacheTTL
	if ttl <= 0 {
		ttl = 9 * time.Minute
	}

	a.mu.Lock()
	if !a.cachedAt.IsZero() && time.Since(a.cachedAt) < ttl && len(a.cached) > 0 {
		out := append([]string(nil), a.cached...)
		a.mu.Unlock()
		return out
	}
	a.mu.Unlock()

	top := a.Top
	if top <= 0 {
		top = 50
	}

	systemPrompt := aiScorerSystemPrompt
	userPrompt := buildAIScorerUserPrompt(entries, top)

	logger.Infof("🔭 [scanner/ai] querying model on %d-symbol universe for top %d (~%dk prompt chars)",
		len(entries), top, len(userPrompt)/1000)

	start := time.Now()
	resp, err := a.Client.CallWithMessages(systemPrompt, userPrompt)
	if err != nil {
		logger.Warnf("🔭 [scanner/ai] call failed (%v) after %s — falling back to rule scorer",
			err, time.Since(start).Truncate(time.Millisecond))
		return a.Fallback.Rank(entries)
	}

	universe := make(map[string]bool, len(entries))
	for _, e := range entries {
		universe[e.Symbol] = true
	}
	picked := parseAISymbolResponse(resp, universe)
	if len(picked) < top/2 {
		logger.Warnf("🔭 [scanner/ai] only %d valid symbols in response (wanted %d) — falling back",
			len(picked), top)
		return a.Fallback.Rank(entries)
	}

	// Compose final ranking: AI picks first, then rule-scorer's ordering for
	// anything the AI didn't include. Downstream hysteresis + position
	// protection still need a fully-ranked universe to choose grace slots.
	out := append([]string(nil), picked...)
	pickedSet := make(map[string]bool, len(picked))
	for _, s := range picked {
		pickedSet[s] = true
	}
	for _, s := range a.Fallback.Rank(entries) {
		if !pickedSet[s] {
			out = append(out, s)
		}
	}

	a.mu.Lock()
	a.cached = out
	a.cachedAt = time.Now()
	a.mu.Unlock()

	logger.Infof("🔭 [scanner/ai] AI picked %d symbols in %s (e.g. %s)",
		len(picked), time.Since(start).Truncate(time.Millisecond), preview(picked, 5))
	return out
}

// aiScorerSystemPrompt explains the task and the strict output contract.
//
// The "richer fields" (OI, OI deltas, range_pos) appear only when the
// scanner is configured with an Enricher and the row survived the
// prefilter. Most rows will lack them — the prompt doesn't promise they're
// present on every line, only that they appear when available.
const aiScorerSystemPrompt = `You are a market scanner picking the most interesting USDT-margined perpetual futures for aggressive momentum trading right now.

Input: a universe of every Binance USDT-M perp with these per-symbol fields:
- price
- 10-minute, 30-minute, 1-hour, 24-hour percent price changes
- 24h quote volume in USDT
- last published funding rate (decimal; 0.0001 = 0.01% per 8h)

Some rows additionally carry:
- open interest (in base coin units) and 10-minute / 1-hour OI percent change
- range_pos (0..1, where the price sits inside the 24h high/low band)

These richer rows came through a more expensive enrichment pass, so when
present they're the strongest signal you have — use them.

Pick the N most interesting tickers for a trader pursuing 10% daily ROI on perp momentum.

What "interesting" means:
- Strong recent momentum (large positive |Δ10m| / |Δ30m| / |Δ1h|) with volume conviction (high quote volume confirms it isn't a thin-book wick).
- Funding rate dislocations (e.g. extreme positive funding while price climbs = crowded longs at risk; extreme negative = squeeze-prone).
- OI confirms momentum: rising price + rising OI = new money entering (real trend); rising price + falling OI = short cover (often fades). The opposite for downtrends.
- Range position: range_pos near 0.95+ with strong Δ24h = breakout candidate; near 0.05 with strong negative Δ24h = breakdown candidate; mid-range with high momentum = continuation through resistance.
- Recent breakouts of multi-day ranges (use 24h % alongside short-window deltas to spot continuation).
- AVOID stablecoin pairs (USDCUSDT, USDPUSDT, etc — by definition zero momentum).
- AVOID extreme low volume (< $5M / 24h) — too easy to manipulate.

Strict output contract:
- Exactly N comma-separated symbols, in your preferred order (best first).
- USDT-suffixed (e.g. "BTCUSDT").
- No commentary, no markdown, no fences. The first character of your response must be a symbol.

If you cannot meaningfully rank N candidates from the data, return as many as you can. The framework treats fewer-than-half results as a failure and falls back to a rule-based scorer; reaching for filler hurts you.`

// buildAIScorerUserPrompt formats the universe in a compact one-line-per-symbol
// shape. Each line is ~80-100 chars; 561 entries → ~48 KB → ~12k tokens.
func buildAIScorerUserPrompt(entries []UniverseEntry, top int) string {
	// Pre-sort by 24h volume desc so the prompt order matches what a human
	// scanner usually reads first. Also helps the model anchor on big names
	// without missing small movers (it sees the whole list anyway).
	sorted := append([]UniverseEntry(nil), entries...)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].QuoteVolume24h > sorted[j].QuoteVolume24h
	})

	var sb strings.Builder
	sb.Grow(len(sorted) * 120)
	sb.WriteString(fmt.Sprintf("Pick the top %d most interesting USDT-M perps. Output exactly %d comma-separated symbols.\n\n", top, top))
	sb.WriteString("Universe (sorted by 24h volume desc):\n")
	for _, e := range sorted {
		sb.WriteString(fmt.Sprintf(
			"%s  $%s  Δ10m=%+0.2f%%  Δ30m=%+0.2f%%  Δ1h=%+0.2f%%  Δ24h=%+0.2f%%  vol=$%s  fund=%+0.4f%%",
			padSymbol(e.Symbol),
			formatPrice(e.Price),
			e.PriceChange10m, e.PriceChange30m, e.PriceChange1h, e.PriceChange24h,
			formatVolume(e.QuoteVolume24h),
			e.FundingRate*100,
		))
		// Append enrichment fields only when present. Zero OI = not enriched
		// (Enricher couldn't fetch this symbol or it's outside the prefilter
		// top K). Keeping enriched rows visually distinct helps the model
		// weight them more heavily.
		if e.OpenInterest > 0 {
			sb.WriteString(fmt.Sprintf(
				"  oi=%s  oi10m=%+0.2f%%  oi1h=%+0.2f%%  rangepos=%0.2f",
				formatOI(e.OpenInterest, e.Price),
				e.OIChg10m, e.OIChg1h,
				e.RangePos(),
			))
		}
		sb.WriteByte('\n')
	}
	sb.WriteString(fmt.Sprintf("\nOutput %d comma-separated symbols now (best first):", top))
	return sb.String()
}

// formatOI reduces base-coin OI × current price to a $B/$M/$K notional, the
// same compact bracket the volume formatter uses. Comparing OI in dollar
// terms across symbols is more meaningful than comparing raw base-coin units.
func formatOI(oiBase, price float64) string {
	notional := oiBase * price
	return "$" + formatVolume(notional)
}

// parseAISymbolResponse extracts up to len(universe) USDT-suffixed symbols
// from the model output. Tolerates commas, whitespace, line breaks, and the
// occasional stray sentence — we just walk through token-by-token and keep
// anything that looks like a symbol AND exists in the universe.
func parseAISymbolResponse(raw string, universe map[string]bool) []string {
	// Replace common separators with commas, then split.
	clean := strings.NewReplacer(
		"\n", ",", "\r", ",", "\t", ",",
		";", ",", " ", ",", "`", "",
		"*", "", "-", "",
	).Replace(raw)
	parts := strings.Split(clean, ",")

	seen := make(map[string]bool, len(parts))
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		sym := strings.ToUpper(strings.TrimSpace(p))
		// Strip trailing punctuation (".", "!", "?", ":") that often follows a
		// symbol when the model wraps its picks in a sentence.
		sym = strings.TrimRight(sym, ".!?:'\"")
		if sym == "" || !strings.HasSuffix(sym, "USDT") {
			continue
		}
		if !universe[sym] {
			continue // hallucinated symbol
		}
		if seen[sym] {
			continue
		}
		seen[sym] = true
		out = append(out, sym)
	}
	return out
}

// padSymbol right-pads a symbol to 12 characters for column alignment in the
// prompt. Helps the model parse the universe list as a table.
func padSymbol(sym string) string {
	const width = 12
	if len(sym) >= width {
		return sym
	}
	return sym + strings.Repeat(" ", width-len(sym))
}

// formatPrice produces a compact price string. Big numbers get no decimals;
// small numbers get enough precision to be meaningful.
func formatPrice(p float64) string {
	switch {
	case p >= 1000:
		return fmt.Sprintf("%.0f", p)
	case p >= 1:
		return fmt.Sprintf("%.2f", p)
	case p >= 0.01:
		return fmt.Sprintf("%.4f", p)
	default:
		return fmt.Sprintf("%.6f", p)
	}
}

// formatVolume condenses 24h quote volume to a token-cheap human form.
// $1.2B / $850M / $42M / $5K — saving ~5 chars per line × 561 lines = ~3k chars.
func formatVolume(v float64) string {
	switch {
	case v >= 1_000_000_000:
		return fmt.Sprintf("%.1fB", v/1_000_000_000)
	case v >= 1_000_000:
		return fmt.Sprintf("%.0fM", v/1_000_000)
	case v >= 1_000:
		return fmt.Sprintf("%.0fK", v/1_000)
	default:
		return fmt.Sprintf("%.0f", v)
	}
}
