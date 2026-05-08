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

// aiScorerSystemPrompt v2 — assumes every row reaching the model has been
// fully enriched (OI history, kline-derived MACD/RSI/ATR on 1h + 4h,
// top-trader L/S ratio). Universe is now ~100 candidates not 561; per-row
// information density is much higher, AI throughput much lower.
const aiScorerSystemPrompt = `You are a market scanner picking the most interesting USDT-margined perpetual futures for aggressive momentum trading targeting 10% daily ROI on compound growth.

Input: per-coin block with these fields, all from the last 24h (1h-period series) plus 4h-timeframe context:

  Header:    price, vol(24h), Δ10m / Δ30m / Δ1h / Δ24h, range_pos (0..1 inside 24h high/low band), funding rate
  OI:        latest, 24-point hourly history, Δ1h / Δ4h / Δ24h percent change
  1h indicators: MACD line, RSI(14), ATR(14), recent OHLC tail (last 6 candles)
  4h indicators: MACD line, RSI(14), ATR(14), recent OHLC tail (last 6 candles)
  L/S:       top-trader position long/short ratio, latest + 24h series
  Bypass:    "(bypass)" tag = made the candidate set on a |Δ1h| > 10% rule, not on volume

Pick the N most interesting symbols. "Interesting" means high probability of a clean directional move within the next 1-4 hours, not just movement that already happened.

Strong signals (any combination wins):
- Aligned momentum across timeframes: 1h MACD turning positive, 4h MACD already positive, 4h RSI rising through 50 → real continuation setup.
- OI confirms direction: rising price + rising OI = new money entering; rising price + falling OI = short cover (fades). Same logic mirrored for shorts.
- Range breakout with conviction: range_pos > 0.95 with Δ24h > +5%, OI rising, funding still neutral = early breakout, not late.
- L/S rotation: top-trader L/S ratio rising on a coin showing +ve momentum = whales adding longs. Falling L/S on a downtrend = whales pressing shorts.
- Funding dislocation: funding > +0.05% per 8h = crowded longs (pullback risk); funding < -0.03% = squeeze-prone (mean-revert candidate).
- Bypass tags: a coin without volume backing but big Δ1h is the riskiest, highest-edge bucket — only pick if multiple confirmations align.

Hard avoids:
- Stablecoin pairs (USDCUSDT, USDPUSDT, etc — zero by construction).
- Coins with degenerate indicators (ATR ≈ 0 = not moving, RSI exactly 50 from no data).
- Late-cycle exhaustion: RSI > 80 on both 1h AND 4h with funding > +0.1% = roof.

Strict output contract:
- Exactly N comma-separated symbols, your preferred order (best first).
- USDT-suffixed.
- No commentary, no markdown, no code fences. First character must be a symbol.

If you can't ground N picks in the data, return fewer. The framework treats fewer-than-half as failure and falls back to a deterministic rule scorer — so filler hurts you.`

// buildAIScorerUserPrompt formats every enriched candidate into a structured
// per-coin block. Layout aims for ~600-800 chars/coin; 100 coins → ~70 KB →
// ~17.5k tokens. Symbols arrive in volume-desc order so big names anchor
// the model's reading without burying small movers (which the bypass tag
// is meant to surface).
func buildAIScorerUserPrompt(entries []UniverseEntry, top int) string {
	sorted := append([]UniverseEntry(nil), entries...)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].QuoteVolume24h > sorted[j].QuoteVolume24h
	})

	var sb strings.Builder
	sb.Grow(len(sorted) * 700)
	sb.WriteString(fmt.Sprintf(
		"Pick the top %d most interesting USDT-M perps from the %d candidates below. Output exactly %d comma-separated symbols.\n\n",
		top, len(sorted), top,
	))
	for _, e := range sorted {
		sb.WriteString(formatEntryBlock(e))
		sb.WriteString("\n")
	}
	sb.WriteString(fmt.Sprintf("Output %d comma-separated symbols now (best first):", top))
	return sb.String()
}

// formatEntryBlock renders one candidate as a labelled multi-line block.
// Order chosen so the model can scan top-down: identifier → price action
// → OI → 1h/4h technicals → L/S. Empty / missing sections are skipped so
// the model doesn't confuse "no data" with "data says zero".
func formatEntryBlock(e UniverseEntry) string {
	var sb strings.Builder
	tag := ""
	if e.ByPass {
		tag = " (bypass)"
	}
	sb.WriteString(fmt.Sprintf("=== %s%s ===\n", e.Symbol, tag))
	sb.WriteString(fmt.Sprintf(
		"Price=$%s  Vol24h=$%s  Range=%0.2f  Δ10m=%+0.2f%%  Δ30m=%+0.2f%%  Δ1h=%+0.2f%%  Δ24h=%+0.2f%%  Fund=%+0.4f%%\n",
		formatPrice(e.Price),
		formatVolume(e.QuoteVolume24h),
		e.RangePos(),
		e.PriceChange10m, e.PriceChange30m, e.PriceChange1h, e.PriceChange24h,
		e.FundingRate*100,
	))

	if e.OpenInterest > 0 {
		sb.WriteString(fmt.Sprintf(
			"OI=%s  Δ1h=%+0.2f%%  Δ4h=%+0.2f%%  Δ24h=%+0.2f%%",
			formatOI(e.OpenInterest, e.Price),
			e.OIChg1h, e.OIChg4h, e.OIChg24h,
		))
		if len(e.OIHistory) >= 4 {
			// Compress to 6 evenly-spaced samples — gives the model "shape"
			// without flooding the prompt with 24 floats.
			sb.WriteString("  hist=")
			sb.WriteString(compressFloats(e.OIHistory, 6))
		}
		sb.WriteString("\n")
	}

	if len(e.Klines1h) >= 26 {
		sb.WriteString(fmt.Sprintf(
			"1h: MACD=%.4f  RSI14=%0.1f  ATR14=%.4f  ohlc(last6)=%s\n",
			e.MACD1h, e.RSI1h, e.ATR1h,
			compressOHLC(e.Klines1h, 6),
		))
	}
	if len(e.Klines4h) >= 26 {
		sb.WriteString(fmt.Sprintf(
			"4h: MACD=%.4f  RSI14=%0.1f  ATR14=%.4f  ohlc(last6)=%s\n",
			e.MACD4h, e.RSI4h, e.ATR4h,
			compressOHLC(e.Klines4h, 6),
		))
	}

	if len(e.LongShortHistory) >= 2 {
		series := make([]float64, 0, len(e.LongShortHistory))
		for _, r := range e.LongShortHistory {
			series = append(series, r.Ratio)
		}
		sb.WriteString(fmt.Sprintf(
			"L/S(top)=%0.3f  hist=%s\n",
			e.LongShortLatest,
			compressFloats(series, 6),
		))
	}

	return sb.String()
}

// compressFloats picks `n` evenly-spaced samples from values and renders
// them as comma-separated. Always includes first + last so the model can
// read direction. Returns an empty string when input is too short.
func compressFloats(values []float64, n int) string {
	if len(values) == 0 || n <= 0 {
		return ""
	}
	if len(values) <= n {
		out := make([]string, len(values))
		for i, v := range values {
			out[i] = trimFloat(v)
		}
		return strings.Join(out, ",")
	}
	out := make([]string, 0, n)
	step := float64(len(values)-1) / float64(n-1)
	for i := 0; i < n; i++ {
		idx := int(float64(i) * step)
		if idx >= len(values) {
			idx = len(values) - 1
		}
		out = append(out, trimFloat(values[idx]))
	}
	return strings.Join(out, ",")
}

// compressOHLC takes the last `n` candles and renders them as O/H/L/C
// quadruplets joined by " | ". Volume omitted to keep the prompt compact —
// the header already carries 24h vol, and per-candle volume rarely changes
// the read.
func compressOHLC(klines []Kline, n int) string {
	if len(klines) == 0 || n <= 0 {
		return ""
	}
	if len(klines) > n {
		klines = klines[len(klines)-n:]
	}
	parts := make([]string, 0, len(klines))
	for _, k := range klines {
		parts = append(parts, fmt.Sprintf(
			"%s/%s/%s/%s",
			trimFloat(k.Open), trimFloat(k.High), trimFloat(k.Low), trimFloat(k.Close),
		))
	}
	return strings.Join(parts, " | ")
}

// trimFloat formats a float without trailing zeros, picking precision based
// on magnitude (matches formatPrice but avoids the leading "$").
func trimFloat(v float64) string {
	switch {
	case v >= 1000:
		return fmt.Sprintf("%.0f", v)
	case v >= 1:
		return fmt.Sprintf("%.2f", v)
	case v >= 0.01:
		return fmt.Sprintf("%.4f", v)
	default:
		return fmt.Sprintf("%.6f", v)
	}
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
