package kernel

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"nofx/logger"
	"nofx/mcp"
	"nofx/store"
)

// StrategyOptimizer runs a meta-AI loop that reviews a trader's recent
// performance and proposes minimal, bounded mutations to its strategy
// config. The user provides a high-level role (via custom_prompt or
// role_definition); everything else can be left at defaults and the
// optimizer refines over time.
//
// The optimizer is **conservative by design**:
//   - Runs at most every EveryNCycles cycles (default 10 → ~30min at 3min cycle)
//   - Only mutates a small whitelist of fields (min_confidence,
//     excluded_coins, custom_prompt_append). Risk caps (leverage,
//     max_positions, position_value_ratio) are off-limits — those
//     belong to the user's risk decision, not the AI's.
//   - Bounds-checks every value before applying.
//   - All mutations logged for auditability.
type StrategyOptimizer struct {
	Store         *store.Store
	AIClient      mcp.AIClient
	UserID        string
	StrategyID    string
	TraderID      string
	EveryNCycles  int           // 0 → disabled
	MinInterval   time.Duration // hard floor between reviews regardless of cycle count
	MaxLookback   int           // recent decisions/orders to feed the optimizer

	lastReviewCycle int
	lastReviewAt    time.Time
}

// optimizerSuggestion is the JSON shape we ask the meta-AI to produce.
// Every field is optional; absent or null means "keep current".
//
// Bounds are intentionally generous (caller wanted more knobs unlocked) but
// every numeric field still clamps in applyBounded so a runaway suggestion
// can't, say, set leverage to 100x or zero out min_confidence.
type optimizerSuggestion struct {
	Reasoning           string   `json:"reasoning"`
	MinConfidence       *int     `json:"min_confidence,omitempty"`
	ExcludedCoinsAdd    []string `json:"excluded_coins_add,omitempty"`
	ExcludedCoinsRemove []string `json:"excluded_coins_remove,omitempty"`
	CustomPromptAppend  string   `json:"custom_prompt_append,omitempty"`

	// Risk caps — bounded but adjustable. Letting the optimizer touch
	// leverage and position sizing is the price of pursuing aggressive
	// objectives like "maximise daily ROI"; the user opted in.
	BTCETHLeverage       *int     `json:"btc_eth_max_leverage,omitempty"`
	AltcoinLeverage      *int     `json:"altcoin_max_leverage,omitempty"`
	MaxPositions         *int     `json:"max_positions,omitempty"`
	BTCETHPositionRatio  *float64 `json:"btc_eth_max_position_value_ratio,omitempty"`
	AltcoinPositionRatio *float64 `json:"altcoin_max_position_value_ratio,omitempty"`
	MinRiskRewardRatio   *float64 `json:"min_risk_reward_ratio,omitempty"`

	// Universe sizing.
	BinanceTopLimit *int `json:"binance_top_limit,omitempty"`

	// Indicator toggles — flipping these reshapes the per-cycle prompt
	// (more or less data fed to Claude). Bool pointers so the optimizer
	// can leave them unchanged.
	EnableEMA  *bool `json:"enable_ema,omitempty"`
	EnableMACD *bool `json:"enable_macd,omitempty"`
	EnableRSI  *bool `json:"enable_rsi,omitempty"`
	EnableATR  *bool `json:"enable_atr,omitempty"`
	EnableBOLL *bool `json:"enable_boll,omitempty"`
}

// MaybeReview triggers a review when both the cycle counter and the wall-
// clock floor allow it. Safe to call every cycle; 99% of calls return early.
// Returns nil on no-op or successful update; errors are surfaced for logging
// only — the trading loop should never abort because optimization failed.
func (o *StrategyOptimizer) MaybeReview(currentCycle int) error {
	if o == nil || o.EveryNCycles <= 0 || o.AIClient == nil || o.Store == nil {
		return nil
	}
	if currentCycle-o.lastReviewCycle < o.EveryNCycles {
		return nil
	}
	if !o.lastReviewAt.IsZero() && time.Since(o.lastReviewAt) < o.MinInterval {
		return nil
	}

	if err := o.review(currentCycle); err != nil {
		// Do not advance lastReviewAt on error so we can retry next cycle —
		// but advance lastReviewCycle so retries respect the spacing.
		o.lastReviewCycle = currentCycle
		return err
	}
	o.lastReviewCycle = currentCycle
	o.lastReviewAt = time.Now().UTC()
	return nil
}

// review runs the meta-AI loop end-to-end.
func (o *StrategyOptimizer) review(cycle int) error {
	strategy, err := o.Store.Strategy().Get(o.UserID, o.StrategyID)
	if err != nil {
		return fmt.Errorf("load strategy: %w", err)
	}
	if strategy == nil {
		return fmt.Errorf("strategy %s not found", o.StrategyID)
	}

	cfgPtr, err := strategy.ParseConfig()
	if err != nil {
		return fmt.Errorf("parse strategy config: %w", err)
	}
	cfg := *cfgPtr

	lookback := o.MaxLookback
	if lookback <= 0 {
		lookback = 20
	}

	decisions, _ := o.Store.Decision().GetLatestRecords(o.TraderID, lookback)
	stats, _ := o.Store.Position().GetFullStats(o.TraderID)
	orders, _ := o.Store.Order().GetTraderOrders(o.TraderID, lookback)
	equity, _ := o.Store.Equity().GetLatest(o.TraderID, 50)

	systemPrompt := o.buildSystemPrompt()
	userPrompt := o.buildUserPrompt(&cfg, decisions, stats, orders, equity)

	resp, err := o.AIClient.CallWithMessages(systemPrompt, userPrompt)
	if err != nil {
		return fmt.Errorf("optimizer ai call: %w", err)
	}

	suggestion, err := parseOptimizerSuggestion(resp)
	if err != nil {
		return fmt.Errorf("parse suggestion: %w", err)
	}

	changed := o.applyBounded(&cfg, suggestion)
	if !changed {
		logger.Infof("🧠 [optimizer] cycle %d — no changes (reasoning: %s)", cycle, truncateTo(suggestion.Reasoning, 200))
		return nil
	}

	// Persist via the strategy store: serialise the mutated config back into
	// the row's JSON config blob.
	configJSON, err := json.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("marshal updated config: %w", err)
	}
	strategy.Config = string(configJSON)
	strategy.UpdatedAt = time.Now().UTC()
	if err := o.Store.Strategy().Update(strategy); err != nil {
		return fmt.Errorf("persist strategy: %w", err)
	}

	logger.Infof("🧠 [optimizer] cycle %d — applied changes: %s | reasoning: %s",
		cycle, summariseSuggestion(suggestion), truncateTo(suggestion.Reasoning, 300))
	return nil
}

// applyBounded mutates cfg in-place with the parts of suggestion that pass
// validation. Returns true iff at least one field was actually changed.
//
// Bounds (V2 — wider than V1):
//   - min_confidence: [50, 95]
//   - excluded_coins: dedup + cap at 50 entries; symbols must end with USDT
//   - custom_prompt_append: max 500 chars per call; concatenated total
//     capped at 4000 chars
//   - btc_eth_max_leverage: [1, 25]
//   - altcoin_max_leverage: [1, 20]
//   - max_positions: [1, 10]
//   - btc_eth_max_position_value_ratio: [0.5, 20]
//   - altcoin_max_position_value_ratio: [0.5, 20]
//   - min_risk_reward_ratio: [1.0, 5.0]
//   - binance_top_limit: [5, MaxCandidateCoins]
//   - indicator toggles: any bool
func (o *StrategyOptimizer) applyBounded(cfg *store.StrategyConfig, s optimizerSuggestion) bool {
	changed := false

	if s.MinConfidence != nil {
		v := clampInt(*s.MinConfidence, 50, 95)
		if v != cfg.RiskControl.MinConfidence {
			cfg.RiskControl.MinConfidence = v
			changed = true
		}
	}

	if s.BTCETHLeverage != nil {
		v := clampInt(*s.BTCETHLeverage, 1, 25)
		if v != cfg.RiskControl.BTCETHMaxLeverage {
			cfg.RiskControl.BTCETHMaxLeverage = v
			changed = true
		}
	}
	if s.AltcoinLeverage != nil {
		v := clampInt(*s.AltcoinLeverage, 1, 20)
		if v != cfg.RiskControl.AltcoinMaxLeverage {
			cfg.RiskControl.AltcoinMaxLeverage = v
			changed = true
		}
	}
	if s.MaxPositions != nil {
		v := clampInt(*s.MaxPositions, 1, 10)
		if v != cfg.RiskControl.MaxPositions {
			cfg.RiskControl.MaxPositions = v
			changed = true
		}
	}
	if s.BTCETHPositionRatio != nil {
		v := clampFloat(*s.BTCETHPositionRatio, 0.5, 20)
		if v != cfg.RiskControl.BTCETHMaxPositionValueRatio {
			cfg.RiskControl.BTCETHMaxPositionValueRatio = v
			changed = true
		}
	}
	if s.AltcoinPositionRatio != nil {
		v := clampFloat(*s.AltcoinPositionRatio, 0.5, 20)
		if v != cfg.RiskControl.AltcoinMaxPositionValueRatio {
			cfg.RiskControl.AltcoinMaxPositionValueRatio = v
			changed = true
		}
	}
	if s.MinRiskRewardRatio != nil {
		v := clampFloat(*s.MinRiskRewardRatio, 1.0, 5.0)
		if v != cfg.RiskControl.MinRiskRewardRatio {
			cfg.RiskControl.MinRiskRewardRatio = v
			changed = true
		}
	}

	if s.BinanceTopLimit != nil {
		v := clampInt(*s.BinanceTopLimit, 5, store.MaxCandidateCoins)
		if v != cfg.CoinSource.BinanceTopLimit {
			cfg.CoinSource.BinanceTopLimit = v
			changed = true
		}
	}

	if s.EnableEMA != nil && *s.EnableEMA != cfg.Indicators.EnableEMA {
		cfg.Indicators.EnableEMA = *s.EnableEMA
		changed = true
	}
	if s.EnableMACD != nil && *s.EnableMACD != cfg.Indicators.EnableMACD {
		cfg.Indicators.EnableMACD = *s.EnableMACD
		changed = true
	}
	if s.EnableRSI != nil && *s.EnableRSI != cfg.Indicators.EnableRSI {
		cfg.Indicators.EnableRSI = *s.EnableRSI
		changed = true
	}
	if s.EnableATR != nil && *s.EnableATR != cfg.Indicators.EnableATR {
		cfg.Indicators.EnableATR = *s.EnableATR
		changed = true
	}
	if s.EnableBOLL != nil && *s.EnableBOLL != cfg.Indicators.EnableBOLL {
		cfg.Indicators.EnableBOLL = *s.EnableBOLL
		changed = true
	}

	if len(s.ExcludedCoinsAdd) > 0 || len(s.ExcludedCoinsRemove) > 0 {
		set := make(map[string]bool, len(cfg.CoinSource.ExcludedCoins))
		for _, sym := range cfg.CoinSource.ExcludedCoins {
			set[sym] = true
		}
		for _, sym := range s.ExcludedCoinsAdd {
			sym = strings.ToUpper(strings.TrimSpace(sym))
			if !strings.HasSuffix(sym, "USDT") || sym == "USDT" {
				continue
			}
			set[sym] = true
		}
		for _, sym := range s.ExcludedCoinsRemove {
			sym = strings.ToUpper(strings.TrimSpace(sym))
			delete(set, sym)
		}
		newList := make([]string, 0, len(set))
		for sym := range set {
			newList = append(newList, sym)
		}
		if len(newList) > 50 {
			newList = newList[:50]
		}
		// Same elements, possibly different order — only mark changed if the
		// set differs from current.
		if !sameStringSet(newList, cfg.CoinSource.ExcludedCoins) {
			cfg.CoinSource.ExcludedCoins = newList
			changed = true
		}
	}

	if s.CustomPromptAppend != "" {
		appended := strings.TrimSpace(s.CustomPromptAppend)
		if len(appended) > 500 {
			appended = appended[:500]
		}
		if appended != "" {
			joined := strings.TrimSpace(cfg.CustomPrompt + "\n\n[auto-tuned " + time.Now().UTC().Format("2006-01-02 15:04Z") + "] " + appended)
			// Cap at 4000 chars total so the system prompt budget stays sane.
			if len(joined) > 4000 {
				joined = joined[len(joined)-4000:]
			}
			if joined != cfg.CustomPrompt {
				cfg.CustomPrompt = joined
				changed = true
			}
		}
	}

	return changed
}

// buildSystemPrompt explains the optimizer's job and output contract.
func (o *StrategyOptimizer) buildSystemPrompt() string {
	return `You are a meta-controller that reviews an AI trader's recent performance and suggests config tweaks. The trader uses your suggestions to adapt over time.

Your job:
1. Read the user's stated objective (the success metric and target).
2. Read the user's stated role (their mandate for the trader).
3. Read the recent decisions, fills, equity curve, and current config.
4. Decide what config changes would move the trader closer to the objective.

Strict output contract — JSON only:

{
  "reasoning": "<2-4 sentences explaining the trajectory vs target and why these changes>",

  // Quality / selection knobs
  "min_confidence":          <int 50-95>,
  "excluded_coins_add":      ["<SYMBOLUSDT>", ...],
  "excluded_coins_remove":   ["<SYMBOLUSDT>", ...],
  "custom_prompt_append":    "<short note for trader prompt; max 500 chars>",

  // Risk / sizing
  "btc_eth_max_leverage":              <int 1-25>,
  "altcoin_max_leverage":              <int 1-20>,
  "max_positions":                     <int 1-10>,
  "btc_eth_max_position_value_ratio":  <float 0.5-20>,
  "altcoin_max_position_value_ratio":  <float 0.5-20>,
  "min_risk_reward_ratio":             <float 1.0-5.0>,

  // Universe size (number of top-by-volume coins fed to AI per cycle)
  "binance_top_limit":  <int 5-30>,

  // Indicator toggles (more data ≠ always better; balance signal vs token cost)
  "enable_ema":  <bool>,
  "enable_macd": <bool>,
  "enable_rsi":  <bool>,
  "enable_atr":  <bool>,
  "enable_boll": <bool>
}

Every field is optional. Output {} if nothing should change.

How to think about it:
- If the objective is aggressive return (e.g. daily_roi 10%) and the trader is under-target with a healthy win-rate, consider raising leverage/position-ratio. If win-rate is bad, raise min_confidence first; size up only after quality improves.
- If drawdown is approaching the hard-stop, cut leverage and max_positions before anything else.
- If certain symbols repeatedly lose, exclude them.
- Indicator toggles: turn on EMA/MACD/RSI when the trader is making clearly bad timing calls; turn them off when they don't help and you want to free up token budget for more candidates.
- custom_prompt_append: distill a single concrete rule per review (e.g. "Open with 30% of normal size; scale up after first +2% move"). Never contradict the role.

Constraints:
- All numeric fields will be clamped to their stated ranges; going outside wastes output.
- excluded_coins must be USDT-suffixed (e.g. "BTCUSDT").
- Be deliberate. Each change should map to specific data in the report — not guesses. The user trusts you to refine, not gamble.
- Output JSON ONLY. No markdown fences, no commentary outside the JSON.`
}

// buildUserPrompt formats the live data the optimizer needs to make a call.
func (o *StrategyOptimizer) buildUserPrompt(cfg *store.StrategyConfig, decisions []*store.DecisionRecord, stats *store.TraderStats, orders []*store.TraderOrder, equity []*store.EquitySnapshot) string {
	var sb strings.Builder

	sb.WriteString("# Objective (the metric we're optimising for)\n\n")
	if cfg.Objective.PrimaryMetric != "" {
		sb.WriteString(fmt.Sprintf("- metric: %s\n- target: %v\n", cfg.Objective.PrimaryMetric, cfg.Objective.PrimaryTarget))
		if cfg.Objective.HorizonDays > 0 {
			sb.WriteString(fmt.Sprintf("- horizon: %d days\n", cfg.Objective.HorizonDays))
		}
		if cfg.Objective.HardStopDrawdownPct > 0 {
			sb.WriteString(fmt.Sprintf("- hard-stop drawdown: %.1f%%\n", cfg.Objective.HardStopDrawdownPct))
		}
		if cfg.Objective.Notes != "" {
			sb.WriteString("- notes: " + cfg.Objective.Notes + "\n")
		}
	} else {
		sb.WriteString("(no explicit objective — fall back to qualitative role-fit review)\n")
	}
	sb.WriteString("\n")

	sb.WriteString("# User's role / mandate\n\n")
	role := strings.TrimSpace(cfg.PromptSections.RoleDefinition)
	if cp := strings.TrimSpace(cfg.CustomPrompt); cp != "" {
		role = role + "\n\nCustom directives:\n" + cp
	}
	if role == "" {
		role = "(none — user has not set a role yet; default behaviour: balanced perp trader)"
	}
	sb.WriteString(role)
	sb.WriteString("\n\n")

	sb.WriteString("# Current editable config (everything below can be tuned)\n\n")
	sb.WriteString(fmt.Sprintf("- min_confidence: %d\n", cfg.RiskControl.MinConfidence))
	sb.WriteString(fmt.Sprintf("- BTC/ETH leverage: %dx, altcoin leverage: %dx\n", cfg.RiskControl.BTCETHMaxLeverage, cfg.RiskControl.AltcoinMaxLeverage))
	sb.WriteString(fmt.Sprintf("- BTC/ETH position ratio: %.2fx equity, altcoin ratio: %.2fx\n", cfg.RiskControl.BTCETHMaxPositionValueRatio, cfg.RiskControl.AltcoinMaxPositionValueRatio))
	sb.WriteString(fmt.Sprintf("- max_positions: %d, min_risk_reward: %.1f\n", cfg.RiskControl.MaxPositions, cfg.RiskControl.MinRiskRewardRatio))
	sb.WriteString(fmt.Sprintf("- binance_top_limit: %d (universe size fed to AI)\n", cfg.CoinSource.BinanceTopLimit))
	sb.WriteString(fmt.Sprintf("- indicators: ema=%t macd=%t rsi=%t atr=%t boll=%t\n",
		cfg.Indicators.EnableEMA, cfg.Indicators.EnableMACD, cfg.Indicators.EnableRSI, cfg.Indicators.EnableATR, cfg.Indicators.EnableBOLL))
	sb.WriteString(fmt.Sprintf("- excluded_coins: %v\n", cfg.CoinSource.ExcludedCoins))
	sb.WriteString(fmt.Sprintf("- custom_prompt: %d chars (last 200: %q)\n\n",
		len(cfg.CustomPrompt), tailString(cfg.CustomPrompt, 200)))

	sb.WriteString("# Structural config (read-only, not tunable here)\n\n")
	sb.WriteString(fmt.Sprintf("- coin_source type: %s\n", cfg.CoinSource.SourceType))
	sb.WriteString(fmt.Sprintf("- timeframes: primary=%s\n\n", cfg.Indicators.Klines.PrimaryTimeframe))

	if stats != nil && stats.TotalTrades > 0 {
		sb.WriteString("# Aggregate trade stats\n\n")
		sb.WriteString(fmt.Sprintf("- total: %d trades, win_rate: %.1f%%\n", stats.TotalTrades, stats.WinRate*100))
		sb.WriteString(fmt.Sprintf("- profit_factor: %.2f, sharpe: %.2f\n", stats.ProfitFactor, stats.SharpeRatio))
		sb.WriteString(fmt.Sprintf("- total_pnl: %+.2f USDT, max_drawdown: %.1f%%\n\n", stats.TotalPnL, stats.MaxDrawdownPct))
	}

	if len(equity) > 0 {
		first := equity[0]
		last := equity[len(equity)-1]
		sb.WriteString(fmt.Sprintf("# Equity (%d snapshots)\n- start: %.2f at %s\n- end: %.2f at %s\n- delta: %+.2f USDT\n\n",
			len(equity), first.TotalEquity, first.Timestamp.Format(time.RFC3339),
			last.TotalEquity, last.Timestamp.Format(time.RFC3339),
			last.TotalEquity-first.TotalEquity))
	}

	if len(decisions) > 0 {
		sb.WriteString("# Recent decisions (most recent first)\n\n")
		for i, d := range decisions {
			if i >= 10 {
				break
			}
			sb.WriteString(fmt.Sprintf("%d. cycle=%d ts=%s success=%v\n", i+1, d.CycleNumber, d.Timestamp.Format(time.RFC3339), d.Success))
			if len(d.Decisions) > 0 {
				for _, a := range d.Decisions {
					ok := "ok"
					if !a.Success {
						ok = "FAIL"
					}
					sb.WriteString(fmt.Sprintf("   → %s %s qty=%.4f @ %.4f (%s)\n",
						a.Action, a.Symbol, a.Quantity, a.Price, ok))
				}
			}
		}
		sb.WriteString("\n")
	}

	if len(orders) > 0 {
		sb.WriteString("# Recent orders (last 10)\n\n")
		for i, ord := range orders {
			if i >= 10 {
				break
			}
			sb.WriteString(fmt.Sprintf("%d. %s %s side=%s qty=%.4f @ %.4f status=%s lev=%d\n",
				i+1, ord.OrderAction, ord.Symbol, ord.Side, ord.Quantity, ord.Price, ord.Status, ord.Leverage))
		}
		sb.WriteString("\n")
	}

	sb.WriteString("# Output JSON now (or {} for no change).")
	return sb.String()
}

// parseOptimizerSuggestion is lenient about preceding/trailing whitespace and
// the occasional fenced block, but strict about the inner JSON shape.
func parseOptimizerSuggestion(raw string) (optimizerSuggestion, error) {
	var out optimizerSuggestion
	body := strings.TrimSpace(raw)

	// Strip markdown fences if the model added them despite instructions.
	if strings.HasPrefix(body, "```") {
		body = strings.TrimPrefix(body, "```json")
		body = strings.TrimPrefix(body, "```")
		body = strings.TrimSuffix(body, "```")
		body = strings.TrimSpace(body)
	}

	// If the model wrapped the JSON in a paragraph, find the first { ... } block.
	if !strings.HasPrefix(body, "{") {
		i := strings.Index(body, "{")
		j := strings.LastIndex(body, "}")
		if i >= 0 && j > i {
			body = body[i : j+1]
		}
	}

	if body == "" {
		return out, fmt.Errorf("empty response")
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		return out, fmt.Errorf("decode: %w (body: %s)", err, truncateTo(body, 200))
	}
	return out, nil
}

func summariseSuggestion(s optimizerSuggestion) string {
	parts := []string{}
	if s.MinConfidence != nil {
		parts = append(parts, fmt.Sprintf("min_confidence=%d", *s.MinConfidence))
	}
	if len(s.ExcludedCoinsAdd) > 0 {
		parts = append(parts, "exclude+="+strings.Join(s.ExcludedCoinsAdd, ","))
	}
	if len(s.ExcludedCoinsRemove) > 0 {
		parts = append(parts, "exclude-="+strings.Join(s.ExcludedCoinsRemove, ","))
	}
	if s.CustomPromptAppend != "" {
		parts = append(parts, fmt.Sprintf("prompt+=%dch", len(s.CustomPromptAppend)))
	}
	if len(parts) == 0 {
		return "(none)"
	}
	return strings.Join(parts, ", ")
}

func sameStringSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	set := make(map[string]bool, len(a))
	for _, s := range a {
		set[s] = true
	}
	for _, s := range b {
		if !set[s] {
			return false
		}
	}
	return true
}

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func clampFloat(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func tailString(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "…" + s[len(s)-n:]
}

func truncateTo(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
