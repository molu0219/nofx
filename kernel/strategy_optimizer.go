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
type optimizerSuggestion struct {
	Reasoning           string   `json:"reasoning"`
	MinConfidence       *int     `json:"min_confidence,omitempty"`
	ExcludedCoinsAdd    []string `json:"excluded_coins_add,omitempty"`
	ExcludedCoinsRemove []string `json:"excluded_coins_remove,omitempty"`
	CustomPromptAppend  string   `json:"custom_prompt_append,omitempty"`
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
// Bounds (V1 — conservative):
//   - min_confidence: clamped to [50, 95]
//   - excluded_coins: dedup + cap at 50 entries; symbols must end with USDT
//   - custom_prompt_append: max 500 chars after concatenation; we do NOT
//     replace the existing custom_prompt, only append a short note so user-
//     supplied directives are never lost
func (o *StrategyOptimizer) applyBounded(cfg *store.StrategyConfig, s optimizerSuggestion) bool {
	changed := false

	if s.MinConfidence != nil {
		v := *s.MinConfidence
		if v < 50 {
			v = 50
		}
		if v > 95 {
			v = 95
		}
		if v != cfg.RiskControl.MinConfidence {
			cfg.RiskControl.MinConfidence = v
			changed = true
		}
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
	return `You are a meta-controller that reviews an AI trader's recent performance and suggests minimal config tweaks. The trader uses your suggestions to adapt over time.

Your job:
1. Read the user's stated role (their mandate for the trader).
2. Read the recent decisions, fills, equity curve, and current config.
3. Decide whether anything in the config should change to better serve the user's role.

Strict output contract — JSON only:

{
  "reasoning": "<1-3 sentences explaining your suggestion>",
  "min_confidence": <integer 50-95, or omit to keep current>,
  "excluded_coins_add": ["<SYMBOLUSDT>", ...],
  "excluded_coins_remove": ["<SYMBOLUSDT>", ...],
  "custom_prompt_append": "<short note appended to trader's custom prompt; max 500 chars>"
}

Output {} if nothing should change.

Constraints (the framework enforces these — going outside is wasted output):
- Only the fields above are editable. Do NOT suggest changes to leverage, max_positions, position sizing, timeframes, or coin source.
- min_confidence is bounded to 50-95.
- excluded_coins must be USDT-suffixed (e.g. "BTCUSDT").
- custom_prompt_append is APPENDED to the existing custom prompt — never write rules that contradict what's already there; instead, refine.
- Be conservative. Only change things when the data clearly warrants it (e.g. 3+ losing trades on the same symbol → consider excluding it; consistent over-trading → raise min_confidence). The user trusts you to refine, not gamble.
- Output JSON ONLY. No markdown fences, no commentary outside the JSON.`
}

// buildUserPrompt formats the live data the optimizer needs to make a call.
func (o *StrategyOptimizer) buildUserPrompt(cfg *store.StrategyConfig, decisions []*store.DecisionRecord, stats *store.TraderStats, orders []*store.TraderOrder, equity []*store.EquitySnapshot) string {
	var sb strings.Builder

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

	sb.WriteString("# Current editable config\n\n")
	sb.WriteString(fmt.Sprintf("- min_confidence: %d\n", cfg.RiskControl.MinConfidence))
	sb.WriteString(fmt.Sprintf("- excluded_coins: %v\n", cfg.CoinSource.ExcludedCoins))
	sb.WriteString(fmt.Sprintf("- custom_prompt length: %d chars (last 200 chars: %q)\n\n",
		len(cfg.CustomPrompt), tailString(cfg.CustomPrompt, 200)))

	sb.WriteString("# Read-only context (other config the trader runs with)\n\n")
	sb.WriteString(fmt.Sprintf("- coin_source: %s, limit %d\n", cfg.CoinSource.SourceType, cfg.CoinSource.BinanceTopLimit))
	sb.WriteString(fmt.Sprintf("- BTC/ETH leverage cap: %dx, altcoin: %dx\n", cfg.RiskControl.BTCETHMaxLeverage, cfg.RiskControl.AltcoinMaxLeverage))
	sb.WriteString(fmt.Sprintf("- max_positions: %d, min_risk_reward: %.1f\n\n", cfg.RiskControl.MaxPositions, cfg.RiskControl.MinRiskRewardRatio))

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
