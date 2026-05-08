package trader

import (
	"context"
	"fmt"
	"github.com/ethereum/go-ethereum/crypto"
	"nofx/kernel"
	"nofx/logger"
	"nofx/market/stream"
	"nofx/mcp"
	_ "nofx/mcp/payment"
	_ "nofx/mcp/provider"
	"nofx/store"
	"nofx/trader/aster"
	"nofx/trader/binance"
	"nofx/trader/bitget"
	"nofx/trader/bybit"
	"nofx/trader/gate"
	"nofx/trader/hyperliquid"
	"nofx/trader/indodax"
	"nofx/trader/kucoin"
	"nofx/trader/lighter"
	"nofx/trader/okx"
	"nofx/trader/paper"
	"nofx/wallet"
	"sync"
	"time"
)

// streamWatcher adapts the stream package's per-symbol subscription helper
// to the paper.LiveTickerOptions.Watcher interface. The package-level
// stream.Watch is a no-op for backends that subscribe globally (Binance
// `!markPrice@arr@1s`) and a real subscribe for per-symbol backends (Bybit).
type streamWatcher struct{}

func (streamWatcher) Watch(symbols ...string) { stream.Watch(symbols...) }

func (at *AutoTrader) logTag() string {
	if at == nil {
		return "[trader_id=unknown]"
	}
	if at.name != "" {
		return fmt.Sprintf("[trader_id=%s trader_name=%s]", at.id, at.name)
	}
	return fmt.Sprintf("[trader_id=%s]", at.id)
}

func (at *AutoTrader) logInfof(format string, args ...interface{}) {
	values := append([]interface{}{at.logTag()}, args...)
	logger.Infof("%s "+format, values...)
}

func (at *AutoTrader) logWarnf(format string, args ...interface{}) {
	values := append([]interface{}{at.logTag()}, args...)
	logger.Warnf("%s "+format, values...)
}

func (at *AutoTrader) logErrorf(format string, args ...interface{}) {
	values := append([]interface{}{at.logTag()}, args...)
	logger.Errorf("%s "+format, values...)
}

// AutoTraderConfig auto trading configuration (simplified version - AI makes all decisions)
type AutoTraderConfig struct {
	// Trader identification
	ID      string // Trader unique identifier (for log directory, etc.)
	Name    string // Trader display name
	AIModel string // AI model: "qwen" or "deepseek"

	// Trading platform selection
	Exchange   string // Exchange type: "binance", "bybit", "okx", "bitget", "gate", "hyperliquid", "aster", "lighter", or "paper"
	ExchangeID string // Exchange account UUID (for multi-account support)

	// Paper trader configuration (used when Exchange == "paper")
	PaperFeeBps  float64 // Taker fee in basis points (default 5 if zero)
	PaperBalance float64 // Starting USDT balance for paper account (defaults to InitialBalance)

	// Binance API configuration
	BinanceAPIKey    string
	BinanceSecretKey string

	// Bybit API configuration
	BybitAPIKey    string
	BybitSecretKey string

	// OKX API configuration
	OKXAPIKey     string
	OKXSecretKey  string
	OKXPassphrase string

	// Bitget API configuration
	BitgetAPIKey     string
	BitgetSecretKey  string
	BitgetPassphrase string

	// Gate API configuration
	GateAPIKey    string
	GateSecretKey string

	// KuCoin API configuration
	KuCoinAPIKey     string
	KuCoinSecretKey  string
	KuCoinPassphrase string

	// Indodax API configuration
	IndodaxAPIKey    string
	IndodaxSecretKey string

	// Hyperliquid configuration
	HyperliquidPrivateKey  string
	HyperliquidWalletAddr  string
	HyperliquidTestnet     bool
	HyperliquidUnifiedAcct bool // Unified Account mode: Spot USDC as Perp collateral

	// Aster configuration
	AsterUser       string // Aster main wallet address
	AsterSigner     string // Aster API wallet address
	AsterPrivateKey string // Aster API wallet private key

	// LIGHTER configuration
	LighterWalletAddr       string // LIGHTER wallet address (L1 wallet)
	LighterPrivateKey       string // LIGHTER L1 private key (for account identification)
	LighterAPIKeyPrivateKey string // LIGHTER API Key private key (40 bytes, for transaction signing)
	LighterAPIKeyIndex      int    // LIGHTER API Key index (0-255)
	LighterTestnet          bool   // Whether to use testnet

	// AI configuration
	UseQwen     bool
	DeepSeekKey string
	QwenKey     string

	// Custom AI API configuration
	CustomAPIURL     string
	CustomAPIKey     string
	CustomModelName  string
	Claw402WalletKey string

	// Scan configuration
	ScanInterval time.Duration // Scan interval (recommended 3 minutes)

	// Account configuration
	InitialBalance float64 // Initial balance (for P&L calculation, must be set manually)

	// Risk control (only as hints, AI can make autonomous decisions)
	MaxDailyLoss    float64       // Maximum daily loss percentage (hint)
	MaxDrawdown     float64       // Maximum drawdown percentage (hint)
	StopTradingTime time.Duration // Pause duration after risk control triggers

	// MaxConsecutiveAIFailures sets how many AI errors in a row trigger a full
	// auto-pause (loop stops, status persists to DB). Safe mode kicks in at 3
	// failures already; this is the harder stop above it. 0 = use the default
	// (DefaultMaxConsecutiveAIFailures).
	MaxConsecutiveAIFailures int

	// Position mode
	IsCrossMargin bool // true=cross margin mode, false=isolated margin mode

	// Competition visibility
	ShowInCompetition bool // Whether to show in competition page

	// Strategy configuration (use complete strategy config)
	StrategyConfig *store.StrategyConfig // Strategy configuration (includes coin sources, indicators, risk control, prompts, etc.)

	// StrategyID is the DB row id for the live config; the auto-optimizer
	// uses it to look up + persist updated settings between cycles. Empty
	// disables optimization regardless of StrategyConfig.AutoOptimize.
	StrategyID string
}

// AutoTrader automatic trader
type AutoTrader struct {
	id                    string // Trader unique identifier
	name                  string // Trader display name
	aiModel               string // AI model name
	exchange              string // Trading platform type (binance/bybit/etc)
	exchangeID            string // Exchange account UUID
	showInCompetition     bool   // Whether to show in competition page
	config                AutoTraderConfig
	trader                Trader // Use Trader interface (supports multiple platforms)
	mcpClient             mcp.AIClient
	store                 *store.Store           // Data storage (decision records, etc.)
	strategyEngine        *kernel.StrategyEngine // Strategy engine (uses strategy configuration)
	cycleNumber           int                    // Current cycle number
	initialBalance        float64
	dailyPnL              float64
	customPrompt          string // Custom trading strategy prompt
	overrideBasePrompt    bool   // Whether to override base prompt
	lastResetTime         time.Time
	stopUntil             time.Time
	isRunning             bool
	isRunningMutex        sync.RWMutex       // Mutex to protect isRunning flag
	startTime             time.Time          // System start time
	callCount             int                // AI call count
	positionFirstSeenTime map[string]int64   // Position first seen time (symbol_side -> timestamp in milliseconds)
	stopMonitorCh         chan struct{}      // Used to stop monitoring goroutine
	monitorWg             sync.WaitGroup     // Used to wait for monitoring goroutine to finish
	peakPnLCache          map[string]float64 // Peak profit cache (symbol -> peak P&L percentage)
	peakPnLCacheMutex     sync.RWMutex       // Cache read-write lock
	lastBalanceSyncTime   time.Time          // Last balance sync time
	userID                string             // User ID
	gridState             *GridState         // Grid trading state (only used when StrategyType == "grid_trading")
	claw402WalletAddr     string             // Claw402 wallet address (derived from private key at start)
	consecutiveAIFailures int                // Consecutive AI call failures
	safeMode              bool               // Safe mode: no new positions, protect existing ones
	safeModeReason        string             // Why safe mode was activated
	autoPaused            bool                       // True when the trader auto-paused itself
	pauseReason           string                     // Human-readable reason — surfaced via /api/status
	notifier              Notifier                   // Optional out-of-band notify channel; nil → no-op
	optimizer             *kernel.StrategyOptimizer  // Optional meta-AI strategy refinement; nil → disabled
	strategyID            string                     // Strategy DB id used by optimizer
}

// NewAutoTrader creates an automatic trader
// st parameter is used to store decision records to database
func NewAutoTrader(config AutoTraderConfig, st *store.Store, userID string) (*AutoTrader, error) {
	// Set default values
	if config.ID == "" {
		config.ID = "default_trader"
	}
	if config.Name == "" {
		config.Name = "Default Trader"
	}
	if config.AIModel == "" {
		if config.UseQwen {
			config.AIModel = "qwen"
		} else {
			config.AIModel = "deepseek"
		}
	}

	// Initialize AI client based on provider
	var mcpClient mcp.AIClient
	aiModel := config.AIModel
	if config.UseQwen && aiModel == "" {
		aiModel = "qwen"
	}

	// Resolve API key (provider-specific overrides)
	apiKey := config.CustomAPIKey
	customURL := config.CustomAPIURL
	switch aiModel {
	case "qwen":
		if config.QwenKey != "" {
			apiKey = config.QwenKey
		}
	case "deepseek", "":
		if config.DeepSeekKey != "" {
			apiKey = config.DeepSeekKey
		}
	}

	// Create client via registry (covers all registered providers)
	if aiModel == "custom" {
		mcpClient = mcp.New()
	} else if aiModel == "" {
		aiModel = "deepseek"
		mcpClient = mcp.NewAIClientByProvider(aiModel)
	} else {
		mcpClient = mcp.NewAIClientByProvider(aiModel)
	}
	if mcpClient == nil {
		mcpClient = mcp.New()
	}

	// Payment providers (claw402) ignore customURL
	switch aiModel {
	case "claw402":
		mcpClient.SetAPIKey(apiKey, "", config.CustomModelName)
	default:
		mcpClient.SetAPIKey(apiKey, customURL, config.CustomModelName)
	}
	logger.Infof("🤖 [%s] Using %s AI", config.Name, aiModel)

	if config.CustomAPIURL != "" || config.CustomModelName != "" {
		logger.Infof("🔧 [%s] Custom config - URL: %s, Model: %s", config.Name, config.CustomAPIURL, config.CustomModelName)
	}

	// Set default trading platform
	if config.Exchange == "" {
		config.Exchange = "binance"
	}

	// Create corresponding trader based on configuration
	var trader Trader
	var err error

	// Record position mode (general)
	marginModeStr := "Cross Margin"
	if !config.IsCrossMargin {
		marginModeStr = "Isolated Margin"
	}
	logger.Infof("📊 [%s] Position mode: %s", config.Name, marginModeStr)

	switch config.Exchange {
	case "binance":
		logger.Infof("🏦 [%s] Using Binance Futures trading", config.Name)
		trader = binance.NewFuturesTrader(config.BinanceAPIKey, config.BinanceSecretKey, userID)
	case "bybit":
		logger.Infof("🏦 [%s] Using Bybit Futures trading", config.Name)
		trader = bybit.NewBybitTrader(config.BybitAPIKey, config.BybitSecretKey)
	case "okx":
		logger.Infof("🏦 [%s] Using OKX Futures trading", config.Name)
		trader = okx.NewOKXTrader(config.OKXAPIKey, config.OKXSecretKey, config.OKXPassphrase)
	case "bitget":
		logger.Infof("🏦 [%s] Using Bitget Futures trading", config.Name)
		trader = bitget.NewBitgetTrader(config.BitgetAPIKey, config.BitgetSecretKey, config.BitgetPassphrase)
	case "gate":
		logger.Infof("🏦 [%s] Using Gate.io Futures trading", config.Name)
		trader = gate.NewGateTrader(config.GateAPIKey, config.GateSecretKey)
	case "kucoin":
		logger.Infof("🏦 [%s] Using KuCoin Futures trading", config.Name)
		trader = kucoin.NewKuCoinTrader(config.KuCoinAPIKey, config.KuCoinSecretKey, config.KuCoinPassphrase)
	case "hyperliquid":
		logger.Infof("🏦 [%s] Using Hyperliquid trading", config.Name)
		trader, err = hyperliquid.NewHyperliquidTrader(config.HyperliquidPrivateKey, config.HyperliquidWalletAddr, config.HyperliquidTestnet, config.HyperliquidUnifiedAcct)
		if err != nil {
			return nil, fmt.Errorf("failed to initialize Hyperliquid trader: %w", err)
		}
	case "aster":
		logger.Infof("🏦 [%s] Using Aster trading", config.Name)
		trader, err = aster.NewAsterTrader(config.AsterUser, config.AsterSigner, config.AsterPrivateKey)
		if err != nil {
			return nil, fmt.Errorf("failed to initialize Aster trader: %w", err)
		}
	case "lighter":
		logger.Infof("🏦 [%s] Using LIGHTER trading", config.Name)

		if config.LighterWalletAddr == "" || config.LighterAPIKeyPrivateKey == "" {
			return nil, fmt.Errorf("Lighter requires wallet address and API Key private key")
		}

		// Lighter only supports mainnet (testnet disabled)
		trader, err = lighter.NewLighterTraderV2(
			config.LighterWalletAddr,
			config.LighterAPIKeyPrivateKey,
			config.LighterAPIKeyIndex,
			false, // Always use mainnet for Lighter
		)
		if err != nil {
			return nil, fmt.Errorf("failed to initialize LIGHTER trader: %w", err)
		}
		logger.Infof("✓ LIGHTER trader initialized successfully")
	case "indodax":
		logger.Infof("🏦 [%s] Using Indodax Spot trading", config.Name)
		trader = indodax.NewIndodaxTrader(config.IndodaxAPIKey, config.IndodaxSecretKey)
	case "paper":
		logger.Infof("📄 [%s] Using paper (virtual) exchange", config.Name)
		paperBalance := config.PaperBalance
		if paperBalance <= 0 {
			paperBalance = config.InitialBalance
		}
		if paperBalance <= 0 {
			paperBalance = 10000 // sensible default starting equity for a paper run
		}
		fee := config.PaperFeeBps
		if fee == 0 {
			fee = 5 // 0.05% taker, generic CEX baseline
		}
		// Pass through the framework store + exchange id so paper state
		// survives restarts. If either is missing, paper.New falls back to
		// pure in-memory mode.
		paperTrader, err := paper.New(paper.Config{
			InitialBalance: paperBalance,
			FeeBps:         fee,
			Store:          st,
			ExchangeID:     config.ExchangeID,
		})
		if err != nil {
			return nil, fmt.Errorf("failed to initialize paper trader: %w", err)
		}
		// Wire the shared websocket mark-price stream so paper trades on
		// sub-second prices. Stop-loss / take-profit / liquidation triggers
		// then fire as soon as the venue publishes a new mark, not 3
		// minutes later when the next decision cycle runs.
		streamMgr := stream.Default()
		paperTrader.StartLiveTicker(context.Background(), paper.LiveTickerOptions{
			Source:  streamMgr,
			Watcher: streamWatcher{},
		})
		trader = paperTrader
	default:
		return nil, fmt.Errorf("unsupported trading platform: %s", config.Exchange)
	}

	// Validate initial balance configuration, auto-fetch from exchange if 0
	if config.InitialBalance <= 0 {
		logger.Infof("📊 [%s] Initial balance not set, attempting to fetch current balance from exchange...", config.Name)
		account, err := trader.GetBalance()
		if err != nil {
			return nil, fmt.Errorf("initial balance not set and unable to fetch balance from exchange: %w", err)
		}
		// Try multiple balance field names (different exchanges return different formats)
		balanceKeys := []string{"total_equity", "totalWalletBalance", "wallet_balance", "totalEq", "balance"}
		var foundBalance float64
		for _, key := range balanceKeys {
			if balance, ok := account[key].(float64); ok && balance > 0 {
				foundBalance = balance
				break
			}
		}
		if foundBalance > 0 {
			config.InitialBalance = foundBalance
			logger.Infof("✓ [%s] Auto-fetched initial balance: %.2f USDT", config.Name, foundBalance)
			// Save to database so it persists across restarts
			if st != nil {
				if err := st.Trader().UpdateInitialBalance(userID, config.ID, foundBalance); err != nil {
					logger.Infof("⚠️  [%s] Failed to save initial balance to database: %v", config.Name, err)
				} else {
					logger.Infof("✓ [%s] Initial balance saved to database", config.Name)
				}
			}
		} else {
			return nil, fmt.Errorf("initial balance must be greater than 0, please set InitialBalance in config or ensure exchange account has balance")
		}
	}

	// Get last cycle number (for recovery)
	var cycleNumber int
	if st != nil {
		cycleNumber, _ = st.Decision().GetLastCycleNumber(config.ID)
		logger.Infof("📊 [%s] Decision records will be stored to database", config.Name)
	}

	// Create strategy engine (must have strategy config)
	if config.StrategyConfig == nil {
		return nil, fmt.Errorf("[%s] strategy not configured", config.Name)
	}
	// Pass claw402 wallet key to strategy engine so nofxos data requests
	// are routed through claw402 (reuses the same wallet as AI calls)
	claw402Key := config.Claw402WalletKey
	if claw402Key == "" && config.AIModel == "claw402" && config.CustomAPIKey != "" {
		claw402Key = config.CustomAPIKey
	}
	strategyEngine := kernel.NewStrategyEngine(config.StrategyConfig, claw402Key)
	logger.Infof("✓ [%s] Using strategy engine (strategy configuration loaded)", config.Name)

	at := &AutoTrader{
		id:                    config.ID,
		name:                  config.Name,
		aiModel:               config.AIModel,
		exchange:              config.Exchange,
		exchangeID:            config.ExchangeID,
		showInCompetition:     config.ShowInCompetition,
		config:                config,
		trader:                trader,
		mcpClient:             mcpClient,
		store:                 st,
		strategyEngine:        strategyEngine,
		cycleNumber:           cycleNumber,
		initialBalance:        config.InitialBalance,
		lastResetTime:         time.Now(),
		startTime:             time.Now(),
		callCount:             0,
		isRunning:             false,
		positionFirstSeenTime: make(map[string]int64),
		stopMonitorCh:         make(chan struct{}),
		monitorWg:             sync.WaitGroup{},
		peakPnLCache:          make(map[string]float64),
		peakPnLCacheMutex:     sync.RWMutex{},
		lastBalanceSyncTime:   time.Now(),
		userID:                userID,
		strategyID:            config.StrategyID,
	}

	// Wire the meta-AI optimizer when the strategy opts in. Off by default;
	// users flip it via Strategy Studio (StrategyConfig.AutoOptimize.Enabled).
	if config.StrategyConfig != nil && config.StrategyConfig.AutoOptimize.Enabled && config.StrategyID != "" && st != nil {
		every := config.StrategyConfig.AutoOptimize.EveryNCycles
		if every <= 0 {
			every = 10
		}
		minIntervalMin := config.StrategyConfig.AutoOptimize.MinIntervalMinutes
		if minIntervalMin <= 0 {
			minIntervalMin = 30
		}
		at.optimizer = &kernel.StrategyOptimizer{
			Store:        st,
			AIClient:     mcpClient,
			UserID:       userID,
			StrategyID:   config.StrategyID,
			TraderID:     config.ID,
			EveryNCycles: every,
			MinInterval:  time.Duration(minIntervalMin) * time.Minute,
			MaxLookback:  20,
		}
		logger.Infof("🧠 [%s] Auto-optimize enabled (every %d cycles, min interval %dmin)",
			config.Name, every, minIntervalMin)
	}

	return at, nil
}

// Run runs the automatic trading main loop
func (at *AutoTrader) Run() error {
	at.isRunningMutex.Lock()
	at.isRunning = true
	at.isRunningMutex.Unlock()

	at.stopMonitorCh = make(chan struct{})
	at.startTime = time.Now()

	logger.Info("🚀 AI-driven automatic trading system started")
	at.logInfof("💰 Initial balance: %.2f USDT", at.initialBalance)
	at.logInfof("⚙️  Scan interval: %v", at.config.ScanInterval)
	logger.Info("🤖 AI will make full decisions on leverage, position size, stop loss/take profit, etc.")

	// Pre-launch checks for claw402 users
	at.runPreLaunchChecks()
	at.monitorWg.Add(1)
	defer at.monitorWg.Done()

	// Start drawdown monitoring
	at.startDrawdownMonitor()

	// Start Lighter order sync if using Lighter exchange
	if at.exchange == "lighter" {
		if lighterTrader, ok := at.trader.(*lighter.LighterTraderV2); ok && at.store != nil {
			lighterTrader.StartOrderSync(at.id, at.exchangeID, at.exchange, at.store, 30*time.Second)
			at.logInfof("🔄 Lighter order+position sync enabled (every 30s)")
		}
	}

	// Start Hyperliquid order sync if using Hyperliquid exchange
	if at.exchange == "hyperliquid" {
		if hyperliquidTrader, ok := at.trader.(*hyperliquid.HyperliquidTrader); ok && at.store != nil {
			hyperliquidTrader.StartOrderSync(at.id, at.exchangeID, at.exchange, at.store, 30*time.Second)
			at.logInfof("🔄 Hyperliquid order+position sync enabled (every 30s)")
		}
	}

	// Start Bybit order sync if using Bybit exchange
	if at.exchange == "bybit" {
		if bybitTrader, ok := at.trader.(*bybit.BybitTrader); ok && at.store != nil {
			bybitTrader.StartOrderSync(at.id, at.exchangeID, at.exchange, at.store, 30*time.Second)
			at.logInfof("🔄 Bybit order+position sync enabled (every 30s)")
		}
	}

	// Start OKX order sync if using OKX exchange
	if at.exchange == "okx" {
		if okxTrader, ok := at.trader.(*okx.OKXTrader); ok && at.store != nil {
			okxTrader.StartOrderSync(at.id, at.exchangeID, at.exchange, at.store, 30*time.Second)
			at.logInfof("🔄 OKX order+position sync enabled (every 30s)")
		}
	}

	// Start Bitget order sync if using Bitget exchange
	if at.exchange == "bitget" {
		if bitgetTrader, ok := at.trader.(*bitget.BitgetTrader); ok && at.store != nil {
			bitgetTrader.StartOrderSync(at.id, at.exchangeID, at.exchange, at.store, 30*time.Second)
			at.logInfof("🔄 Bitget order+position sync enabled (every 30s)")
		}
	}

	// Start Aster order sync if using Aster exchange
	if at.exchange == "aster" {
		if asterTrader, ok := at.trader.(*aster.AsterTrader); ok && at.store != nil {
			asterTrader.StartOrderSync(at.id, at.exchangeID, at.exchange, at.store, 30*time.Second)
			at.logInfof("🔄 Aster order+position sync enabled (every 30s)")
		}
	}

	// Start Binance order sync if using Binance exchange
	if at.exchange == "binance" {
		if binanceTrader, ok := at.trader.(*binance.FuturesTrader); ok && at.store != nil {
			binanceTrader.StartOrderSync(at.id, at.exchangeID, at.exchange, at.store, 30*time.Second)
			at.logInfof("🔄 Binance order+position sync enabled (every 30s)")
		}
	}

	// Start Gate order sync if using Gate exchange
	if at.exchange == "gate" {
		if gateTrader, ok := at.trader.(*gate.GateTrader); ok && at.store != nil {
			gateTrader.StartOrderSync(at.id, at.exchangeID, at.exchange, at.store, 30*time.Second)
			at.logInfof("🔄 Gate order+position sync enabled (every 30s)")
		}
	}

	// Start KuCoin order sync if using KuCoin exchange
	if at.exchange == "kucoin" {
		if kucoinTrader, ok := at.trader.(*kucoin.KuCoinTrader); ok && at.store != nil {
			kucoinTrader.StartOrderSync(at.id, at.exchangeID, at.exchange, at.store, 30*time.Second)
			at.logInfof("🔄 KuCoin order+position sync enabled (every 30s)")
		}
	}

	ticker := time.NewTicker(at.config.ScanInterval)
	defer ticker.Stop()

	// Check if this is a grid trading strategy
	isGridStrategy := at.IsGridStrategy()
	if isGridStrategy {
		at.logInfof("🔲 Grid trading strategy detected, initializing grid...")
		if err := at.InitializeGrid(); err != nil {
			at.logErrorf("❌ Failed to initialize grid: %v", err)
			return fmt.Errorf("grid initialization failed: %w", err)
		}
	}

	// Execute immediately on first run
	if isGridStrategy {
		if err := at.RunGridCycle(); err != nil {
			at.logErrorf("❌ Grid execution failed: %v", err)
		}
	} else {
		if err := at.runCycle(); err != nil {
			at.logErrorf("❌ Execution failed: %v", err)
		}
	}

	for {
		at.isRunningMutex.RLock()
		running := at.isRunning
		at.isRunningMutex.RUnlock()

		if !running {
			break
		}

		select {
		case <-ticker.C:
			if isGridStrategy {
				if err := at.RunGridCycle(); err != nil {
					at.logErrorf("❌ Grid execution failed: %v", err)
				}
			} else {
				if err := at.runCycle(); err != nil {
					at.logErrorf("❌ Execution failed: %v", err)
				}
			}
		case <-at.stopMonitorCh:
			at.logInfof("⏹ Stop signal received, exiting automatic trading main loop")
			return nil
		}
	}

	return nil
}

// SetNotifier wires an out-of-band notifier (Telegram, email, etc.) for
// operator-attention events such as auto-pause. Safe to call before Run.
// Passing nil restores the no-op default.
func (at *AutoTrader) SetNotifier(n Notifier) {
	at.isRunningMutex.Lock()
	defer at.isRunningMutex.Unlock()
	at.notifier = n
}

// GetTrader returns the underlying exchange Trader (paper / binance / etc.)
// the AutoTrader owns. Exposed so close-position and reset paths can act on
// the live state — particularly for paper, where the in-memory positions
// live on this instance and can't be reconstructed from credentials.
func (at *AutoTrader) GetTrader() Trader {
	at.isRunningMutex.RLock()
	defer at.isRunningMutex.RUnlock()
	return at.trader
}

// AutoPauseReason returns the human-readable reason the trader auto-paused
// itself, or the empty string if the trader is running normally / was stopped
// manually. Used by /api/status to surface the cause to the dashboard.
func (at *AutoTrader) AutoPauseReason() string {
	at.isRunningMutex.RLock()
	defer at.isRunningMutex.RUnlock()
	if !at.autoPaused {
		return ""
	}
	return at.pauseReason
}

// IsAutoPaused reports whether the trader stopped itself due to a runtime
// failure (vs. user-initiated Stop()).
func (at *AutoTrader) IsAutoPaused() bool {
	at.isRunningMutex.RLock()
	defer at.isRunningMutex.RUnlock()
	return at.autoPaused
}

// Stop stops the automatic trading
func (at *AutoTrader) Stop() {
	at.isRunningMutex.Lock()
	if !at.isRunning {
		at.isRunningMutex.Unlock()
		return
	}
	at.isRunning = false
	at.isRunningMutex.Unlock()

	close(at.stopMonitorCh) // Notify monitoring goroutine to stop
	at.monitorWg.Wait()     // Wait for monitoring goroutine to finish
	logger.Info("⏹ Automatic trading system stopped")
}

// GetID gets trader ID
// GetInitialBalance returns the configured starting balance, used by the SSE
// stream and elsewhere to compute total P&L percentages.
func (at *AutoTrader) GetInitialBalance() float64 {
	return at.initialBalance
}

func (at *AutoTrader) GetID() string {
	return at.id
}

// GetUnderlyingTrader returns the underlying Trader interface implementation
// This is used by grid trading and other components that need direct exchange access
func (at *AutoTrader) GetUnderlyingTrader() Trader {
	return at.trader
}

// GetName gets trader name
func (at *AutoTrader) GetName() string {
	return at.name
}

// GetAIModel gets AI model
func (at *AutoTrader) GetAIModel() string {
	return at.aiModel
}

// GetExchange gets exchange
func (at *AutoTrader) GetExchange() string {
	return at.exchange
}

// GetShowInCompetition returns whether trader should be shown in competition
func (at *AutoTrader) GetShowInCompetition() bool {
	return at.showInCompetition
}

// SetShowInCompetition sets whether trader should be shown in competition
func (at *AutoTrader) SetShowInCompetition(show bool) {
	at.showInCompetition = show
}

// SetCustomPrompt sets custom trading strategy prompt
func (at *AutoTrader) SetCustomPrompt(prompt string) {
	at.customPrompt = prompt
}

// SetOverrideBasePrompt sets whether to override base prompt
func (at *AutoTrader) SetOverrideBasePrompt(override bool) {
	at.overrideBasePrompt = override
}

// GetSystemPromptTemplate gets current system prompt template name (from strategy config)
func (at *AutoTrader) GetSystemPromptTemplate() string {
	if at.strategyEngine != nil {
		config := at.strategyEngine.GetConfig()
		if config.CustomPrompt != "" {
			return "custom"
		}
	}
	return "strategy"
}

// GetCandidateCoins returns the current candidate coin set from the trader's strategy engine.
func (at *AutoTrader) GetCandidateCoins() ([]kernel.CandidateCoin, error) {
	if at.strategyEngine == nil {
		return nil, fmt.Errorf("strategy engine not configured")
	}
	return at.strategyEngine.GetCandidateCoins()
}

// GetStrategyConfig returns the current strategy config used by the trader.
func (at *AutoTrader) GetStrategyConfig() *store.StrategyConfig {
	if at.strategyEngine == nil {
		return at.config.StrategyConfig
	}
	return at.strategyEngine.GetConfig()
}

// GetStore gets data store (for external access to decision records, etc.)
func (at *AutoTrader) GetStore() *store.Store {
	return at.store
}

// calculatePnLPercentage calculates P&L percentage (based on margin, automatically considers leverage)
// Return rate = Unrealized P&L / Margin x 100%
func calculatePnLPercentage(unrealizedPnl, marginUsed float64) float64 {
	if marginUsed > 0 {
		return (unrealizedPnl / marginUsed) * 100
	}
	return 0.0
}

// runPreLaunchChecks performs pre-launch checks for claw402 users (wallet balance, runway estimate)
func (at *AutoTrader) runPreLaunchChecks() {
	if !store.IsClaw402Config(at.config.AIModel) {
		return
	}

	logger.Info("🔍 Running pre-launch checks (claw402)...")

	// Derive wallet address from CustomAPIKey (which is the private key for claw402)
	if at.config.CustomAPIKey != "" {
		// Try to derive address using go-ethereum
		addr := deriveWalletAddress(at.config.CustomAPIKey)
		if addr != "" {
			at.claw402WalletAddr = addr
			logger.Infof("💳 [%s] Claw402 wallet: %s", at.name, addr)

			// Query USDC balance
			balance, err := wallet.QueryUSDCBalance(addr)
			if err != nil {
				logger.Warnf("⚠️ [%s] Could not query USDC balance: %v", at.name, err)
			} else {
				// Estimate runway
				scanMinutes := int(at.config.ScanInterval.Minutes())
				modelName := at.config.CustomModelName
				if modelName == "" {
					modelName = "deepseek"
				}
				dailyCost, runway := store.EstimateRunway(balance, modelName, scanMinutes)
				logger.Infof("💰 [%s] USDC Balance: $%.2f | Daily AI cost: ~$%.2f | Runway: ~%.1f days",
					at.name, balance, dailyCost, runway)

				if balance < 1.0 {
					logger.Warnf("⚠️ [%s] Low USDC balance! Consider topping up.", at.name)
				}
				if balance <= 0 {
					logger.Errorf("🚨 [%s] USDC balance is ZERO — AI calls will fail!", at.name)
				}
			}
		}
	}

	logger.Info("✅ Pre-launch checks complete")
}

// deriveWalletAddress derives an Ethereum address from a hex private key
func deriveWalletAddress(privateKeyHex string) string {
	// Remove 0x prefix if present
	if len(privateKeyHex) > 2 && privateKeyHex[:2] == "0x" {
		privateKeyHex = privateKeyHex[2:]
	}

	privateKey, err := crypto.HexToECDSA(privateKeyHex)
	if err != nil {
		return ""
	}

	address := crypto.PubkeyToAddress(privateKey.PublicKey)
	return address.Hex()
}
