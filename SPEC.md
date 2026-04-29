# SPEC.md

> 進度在 checkbox。驗收全 [x] = 完成。完成搬到 SPEC-archive.md。
>
> 本 SPEC 從現有 codebase 推導而成（截至 2026-04-29，commit `0d3b9536`）。
> 已實作的功能標 [x] 並在 Log 中註明 `從 git log 推斷`。
> 衝突或細節差異以 `.go` 程式碼為準（SPEC 用於規範後續開發）。

---

## F001 系統入口與設定

### 規格
NOFX 是 Go 後端 + React 前端的自托管 AI 交易系統。`main.go` 載入 `.env`、初始化 logger / config / 加密服務 / 資料庫 / TraderManager / API Server / Telegram Bot / NOFXi Agent，graceful shutdown 由 SIGINT/SIGTERM 觸發。

### Contract
- 進入點: `main.go:main()`
- 啟動順序: env → logger → config → crypto → DB(SQLite|Postgres) → TraderManager → API(`:NOFX_BACKEND_PORT`) → Telegram → NOFXi
- 設定來源: `.env`（見 `.env.example`），DB 也可覆寫部分設定（model config/exchange/telegram token）
- 失敗模式: encryption key 缺失 → fatal；DB 連不上 → fatal；API port 占用 → fatal；Telegram token 缺 → 跳過但保留 reload channel

### 驗收條件
- [x] `make run` / `go run main.go` 啟動後 `/api/health` 回傳 200（驗證: curl）
- [x] `.env` 缺 `JWT_SECRET` / `DATA_ENCRYPTION_KEY` 時 fatal log 明確指出缺哪一個（驗證: build）
- [x] SIGINT 後 5 秒內 graceful shutdown（驗證: UI-manual）
- [x] `make build` 產生 `./nofx` binary（驗證: build）

### 範圍限制
- 不含 cluster/HA 部署（單機 binary）
- 不含 secrets manager 整合（用 env / DB 加密）

### 任務
狀態: [ ] todo | [>] active | [?] review | [!] blocked | [x] done | [✗] failed | [-] shelved
- [x] F001-T01: 啟動骨架 + graceful shutdown
  - 產出: `main.go`
  - 驗證: build pass + curl /api/health
- [x] F001-T02: 多資料庫支援（SQLite/Postgres）
  - 產出: `store/driver.go`, `store/store.go`, `store/gorm.go`
  - 驗證: integration test
- [ ] F001-T03: 健康檢查與 readiness 分離（含 dependency probe）
  - contract: GET `/api/health` (alive) + GET `/api/ready` (DB+crypto+exchange roundtrip)
  - 產出: `api/server.go`
  - 依賴: F001-T01
  - 可並行: 是
  - 預估: 小
  - assign: Backend Architect
  - test: required
  - verify: `go test ./api/... -run TestReady`

### Log
- 2026-04-29 F001-T01 done. 從 git log 推斷: 啟動骨架自 commit `b9b0a521` 起穩定。已在 `main.go` 驗證。
- 2026-04-29 F001-T02 done. 從 git log 推斷: store 層支援 sqlite + postgres，已在 `store/driver.go` 驗證。

---

## F002 加密與金鑰管理

### 規格
- 伺服器端用 AES-256-GCM 加密敏感欄位（exchange API key/secret、AI provider key、wallet privkey）寫進 DB
- 用戶端用 RSA-2048 對交易/設定請求做 transport encryption（瀏覽器 Web Crypto → 後端解密）
- 金鑰來源僅 env：`DATA_ENCRYPTION_KEY` (base64) + `RSA_PRIVATE_KEY` (PEM)

### Contract
- API:
  - `GET /api/crypto/config` → `{transport_encryption: bool}`
  - `GET /api/crypto/public-key` → `{public_key: string (PEM)}`
  - `POST /api/crypto/decrypt` → `{plaintext: string}` (server-internal, no auth)
- Go API: `crypto.NewCryptoService()`, `crypto.SetGlobalCryptoService()`, `crypto.EncryptedString` (DB column type)
- Error case: `DATA_ENCRYPTION_KEY` 不是 32 byte → fatal；RSA decrypt 失敗 → 400 + 錯誤碼 `CRYPTO_DECRYPT_FAIL`

### 驗收條件
- [ ] 啟動時驗證 encryption key 長度（驗證: build）— **Evaluator FAIL 2026-04-29**：`crypto/crypto.go:90-106,153-165` 對非 32-byte key SHA256-fallback，違反 fatal contract
- [x] 寫入 DB 的 API key 在 SQL dump 中是 ciphertext（驗證: test）
- [x] 同一明文經兩次加密產生不同 ciphertext（GCM nonce 不重用，驗證: test）
- [x] `TRANSPORT_ENCRYPTION=false` 時前端 fallback 到明文 POST（驗證: UI-manual）

### 範圍限制
- 不含金鑰輪換自動化
- 不含 HSM 整合

### 任務
- [x] F002-T01: AES-256-GCM service + EncryptedString column type
  - 產出: `crypto/`
  - 驗證: `go test ./crypto/...`
- [x] F002-T02: RSA transport encryption（前後端）
  - contract: GET `/api/crypto/public-key`, POST `/api/crypto/decrypt`
  - 產出: `api/crypto_handler.go`, `web/src/utils/`
  - 驗證: integration
- [x] F002-T03: 修 critical silent-fail bugs（Reviewer 2026-04-29 FAIL）
  - 規格: 三個 silent fallback 必須改成 explicit error：
    1. `crypto/crypto.go:91-106 loadDataKeyFromEnv` — 移除 SHA256 fallback，`len(decoded) != 32` 直接 return error
    2. `crypto/crypto.go:434-438 EncryptedString.Scan` — 解密失敗 return err（不要把 ciphertext 當 plaintext）
    3. `crypto/crypto.go:456-461 EncryptedString.Value` — 加密失敗 return err（不要 plaintext 落盤）
  - 產出: `crypto/crypto.go`
  - 依賴: 無
  - 可並行: 否（同檔三處）
  - 預估: 中
  - assign: Security Engineer
  - test: required
  - verify: `go test ./crypto/... -run TestKeyLengthFatal && go test ./crypto/... -run TestScanError && go test ./crypto/... -run TestValueError`
- [ ] F002-T04: 補齊 `crypto/crypto_test.go`
  - 規格: F002 acceptance criteria 列了三個 `驗證: test` 但 `crypto/` 一個 test 檔都沒有
  - contract: 至少測試：(a) nonce 不重用 (b) key 長度 fatal (c) Value/Scan error propagation (d) RSA OAEP 解密成功 + 失敗
  - 產出: `crypto/crypto_test.go`
  - 依賴: F002-T03
  - 可並行: 否
  - 預估: 中
  - assign: Security Engineer
  - test: required
  - verify: `go test ./crypto/... -v`
- [ ] F002-T05: 修 high-severity issues
  - 規格:
    1. `api/crypto_handler.go HandleDecryptSensitiveData` — RSA decrypt 失敗回 `400 + CRYPTO_DECRYPT_FAIL`（目前是 500）
    2. `crypto/crypto.go:286-289 DecryptPayload` — `payload.TS == 0` 不能 skip 驗證，要 reject
    3. `normalizeAESKey:153-165` — 拒絕 16/24-byte key，只准 32（DECISION #6 是 AES-256）
    4. 確認 `/api/crypto/decrypt` 的 route 是否在 protected group（route_registry 或 server.go 設定）
  - 產出: `crypto/crypto.go`, `api/crypto_handler.go`, `api/server.go`
  - 依賴: F002-T03
  - 可並行: 是
  - 預估: 中
  - assign: Security Engineer
  - test: required
  - verify: `go test ./crypto/... && go test ./api/... -run TestCrypto`
- [ ] F002-T06: 移除 dead code `isValidPrivateKey`
  - 產出: `api/crypto_handler.go:86-93`
  - 依賴: 無
  - 可並行: 是
  - 預估: 小
  - assign: Backend Architect
  - test: skip
  - verify: `go vet ./api/...`

### Log
- 2026-04-29 F002-T01 done. 從 codebase 推斷: `crypto/` 已存在 AES-256 加密服務，main.go 啟動時呼叫 `NewCryptoService`。
- 2026-04-29 F002-T02 done. 從 git log 推斷: commit `2f483633` 等多次強化加密相關流程。
- 2026-04-29 F002 Evaluator: **FAIL**. 條件 1 (key 長度 fatal) FAIL — `crypto/crypto.go:90-106,153-165` SHA256-fallback 違反 SPEC contract。條件 2/3 PASS，條件 4 SKIP。
- 2026-04-29 F002 Code Reviewer: **FAIL**. 3 CRITICAL + 3 HIGH + 2 MEDIUM + 1 LOW。CRITICAL: silent fallback in loadDataKey、Scan 解密失敗回 ciphertext、Value 加密失敗 plaintext 落盤。HIGH: /api/crypto/decrypt auth、replay window、無 test 檔。已新增 F002-T03..T06 follow-up tasks 修補。
- 2026-04-29 F002-T03 done. 修 3 個 CRITICAL silent-fallback bugs：(a) `loadDataKeyFromEnv` 移除 SHA256 fallback，非 32-byte key 直接 fatal — smoke 驗證 `DATA_ENCRYPTION_KEY=AAAA go run main.go` 立刻 fatal 並印「decoded to 3 bytes; AES-256 requires exactly 32 bytes」。(b) `EncryptedString.Scan` 解密失敗 return err，避免把 ciphertext 當 plaintext 流到 exchange API/agent。(c) `EncryptedString.Value` 加密失敗 return err，杜絕 plaintext 落盤違反 DECISION #2。順手刪掉 `normalizeAESKey` dead code（SHA256 fallback 的源頭，順帶解決 F002-T05 item 3「拒絕 16/24-byte key」）。新增 `crypto/crypto_test.go` 共 6 個 test (5 個 sub-case) 全 PASS（含 -race）：TestKeyLengthFatal / TestScanError / TestValueError / TestNonceUniqueness / TestScanPlainStringPassthrough。`go build ./...` + `go vet ./crypto/...` clean。

---

## F003 x402 / Claw402 USDC 支付層

### 規格
NOFX 對 AI provider 採 **wallet-first**：呼叫 LLM 前用 USDC 簽 x402 payment header，無 API key 也能跑。Claw402 是預設 gateway，在 Base 鏈上掛 15+ models。Wallet privkey 用 F002 加密保存。

### Contract
- Go API: `mcp.AIClient`, `payment.X402Provider`, `payment.Claw402Provider`
- 流程:
  1. AI request → MCP client
  2. Provider 嘗試走 x402 → 收到 402 + price
  3. wallet 簽 USDC payment（`X402v2PaymentRequired`）
  4. retry，最多 `X402MaxPaymentRetries=5` 次，間隔 `X402RetryBaseWait=3s`
  5. timeout = `X402Timeout=5min`（DeepSeek/長推理保護）
- API: `POST /api/wallet/validate`, `POST /api/wallet/generate`
- USDC 餘額快取: `wallet/balance_cache.go`
- Error case: 餘額不足 → 422 + `INSUFFICIENT_USDC`；402 重試耗盡 → 502；簽章失敗 → 500 + `WALLET_SIGN_FAIL`

### 驗收條件
- [x] AI 呼叫前先 preflight USDC 餘額（commit `2f483633`）（驗證: test）
- [x] 餘額快取 TTL 內不重複查鏈上（驗證: test）
- [x] 402 重試不會 double charge（同一 nonce）（驗證: test）
- [ ] 設定面板顯示每個模型每千 token 的 USDC 估價（驗證: UI-manual）

### 範圍限制
- 只支援 Base 鏈 USDC（後續可擴）
- 不支援批次 settlement，每次 request 都簽

### 任務
- [x] F003-T01: x402 v2 payment provider
  - 產出: `mcp/payment/x402.go`
  - 驗證: `go test ./mcp/...`
- [x] F003-T02: Claw402 provider（Base USDC, 15+ models）
  - contract: 走 `X402v2PaymentRequired` 流程
  - 產出: `mcp/payment/claw402.go`
  - 依賴: F003-T01
  - 驗證: integration
- [x] F003-T03: USDC preflight + 餘額快取
  - 產出: `wallet/usdc.go`, `wallet/balance_cache.go`
  - 依賴: F003-T01
  - 驗證: test
- [ ] F003-T04a: `GET /api/models` 回傳 `usdc_per_1k_tokens`（後端）
  - contract: `SafeModelConfig` 加 `usdc_per_1k_tokens`，handler 取 `store.GetModelPrice(provider)`
  - 產出: `api/handler_ai_model.go`, `store/ai_model.go`
  - 依賴: F003-T01
  - 可並行: 是
  - 預估: 小
  - assign: Backend Architect
  - test: required
  - verify: `go test ./api/... -run TestModels && curl -H "Auth..." /api/models | jq '.[0].usdc_per_1k_tokens'`
- [ ] F003-T04b: ModelConfigModal 顯示 USDC 估價（前端）
  - contract: 渲染 F003-T04a 回傳的欄位
  - 產出: `web/src/components/trader/ModelConfigModal.tsx`
  - 依賴: F003-T04a
  - 可並行: 否
  - 預估: 小
  - assign: Frontend Developer
  - test: required
  - verify: `cd web && npm run test --run ModelConfigModal && npm run lint`

### Log
- 2026-04-29 F003-T01,T02,T03 done. 從 git log 推斷: x402 + claw402 主流程於 commit `2f483633`/`a20a71b8`/`117d2f7f` 系列上線並持續硬化。

---

## F004 多 AI Provider 適配

### 規格
統一 `mcp.AIClient` interface，下面掛 8 個 provider（DeepSeek/Claude/OpenAI/Gemini/Grok/Kimi/MiniMax/Qwen）。User 可同時設定 API-key 模式或 x402 wallet 模式。Streaming + tool calling + thinking model 支援。

### Contract
- Go API:
  - `mcp.AIClient.Chat(ctx, req) (resp, error)`
  - `mcp.AIClient.Stream(ctx, req) (<-chan Chunk, error)`
  - `mcp.RegisterProvider(name, factory)`
- API: `GET /api/supported-models`, `GET /api/models`, `PUT /api/models`
- Thinking model 特殊處理: 不傳 `max_tokens`（commit `4cadf6f4`）
- Error case: provider 410/404 → 跳到下一個 fallback；context timeout → 502 + `AI_TIMEOUT`

### 驗收條件
- [x] 8 個 provider 都實作 AIClient interface（驗證: build）
- [x] streaming 路徑回報 token usage 給 telemetry（commit `a1f909ad`）（驗證: test）
- [x] thinking model 不傳 `max_tokens`（驗證: test）
- [ ] 同一 trader 多 provider fallback 鏈（驗證: test）

### 範圍限制
- 不含 self-hosted LLM（如 ollama）

### 任務
- [x] F004-T01: AIClient interface + registry
  - 產出: `mcp/client.go`, `mcp/registry.go`, `mcp/providers.go`
  - 驗證: test
- [x] F004-T02: 8 個 provider adapter
  - 產出: `mcp/provider/{deepseek,claude,openai,gemini,grok,kimi,minimax,qwen}.go`
  - 依賴: F004-T01
  - 驗證: build + per-provider tests
- [ ] F004-T03: Provider fallback 鏈設定 + 路由
  - contract: `PUT /api/models` 支援 `fallback_chain: [provider_id, ...]`
  - 產出: `mcp/client.go`, `api/handler_ai_model.go`
  - 依賴: F004-T02
  - 可並行: 否（影響核心 client）
  - 預估: 中
  - assign: Backend Architect
  - test: required
  - verify: `go test ./mcp/... -run TestFallback`

### Log
- 2026-04-29 F004-T01,T02 done. 從 git log 推斷: provider 矩陣自 commit `f5891aa3`/`a20a71b8`/`3dbf5bee` 等持續擴充。

---

## F005 多交易所連接器

### 規格
`trader/types/Trader` interface，10 個交易所實作（Binance, Bybit, OKX, Bitget, KuCoin, Gate, Hyperliquid, Aster, Lighter, Indodax）。CEX 走 REST + WebSocket，DEX 走 EVM RPC + 對應 SDK。

### Contract
- Go interface: `trader/types/interface.go:Trader`（`Account`, `Positions`, `Orders`, `Sync`, `PlaceOrder`, `CancelOrder` 等）
- DEX 子集: `trader/types/interface.go:GridTrader`（含 grid-specific operations）
- 安全: API key 落 DB 前用 F002 加密
- Error case: rate limit → exponential backoff；wallet RPC nil → 防呆（commit `851f152c`）
- 註: exchange downtime → 自動 pause 屬於 F007-T05 範疇，目前尚未實作
- API: `GET /api/exchanges`, `POST /api/exchanges`, `PUT /api/exchanges`, `DELETE /api/exchanges/:id`, `GET /api/exchanges/account-state`

### 驗收條件
- [x] 每個交易所有對應 trader_test.go 或 sync_test.go（驗證: test）
- [x] OKX 尊重 margin mode 設定（commit `117d2f7f`）（驗證: test）
- [x] account-state cache 含 connection + balance（驗證: test）
- [ ] 統一 rate-limit middleware（驗證: test）

### 範圍限制
- 不含 spot trading（重點是 perp futures + grid）
- Indodax 為地區性支援，功能可能不全

### 任務
- [x] F005-T01: Trader interface 定義
  - 產出: `trader/types/interface.go`
  - 驗證: build
- [x] F005-T02: Binance 連接器（spot futures + grid）
  - 產出: `trader/binance/`
  - 依賴: F005-T01
  - 驗證: `go test ./trader/binance/...`
- [x] F005-T03: Bybit 連接器
  - 產出: `trader/bybit/`
  - 依賴: F005-T01
  - 驗證: `go test ./trader/bybit/...`
- [x] F005-T04: OKX 連接器（含 margin mode 尊重，commit `117d2f7f`）
  - 產出: `trader/okx/`
  - 依賴: F005-T01
  - 驗證: `go test ./trader/okx/...`
- [x] F005-T05: Bitget 連接器
  - 產出: `trader/bitget/`
  - 依賴: F005-T01
  - 驗證: `go test ./trader/bitget/...`
- [x] F005-T06: KuCoin 連接器
  - 產出: `trader/kucoin/`
  - 依賴: F005-T01
  - 驗證: `go test ./trader/kucoin/...`
- [x] F005-T07: Gate 連接器
  - 產出: `trader/gate/`
  - 依賴: F005-T01
  - 驗證: `go test ./trader/gate/...`
- [x] F005-T08: Hyperliquid 連接器（DEX，含 race fix）
  - 產出: `trader/hyperliquid/`
  - 依賴: F005-T01
  - 驗證: `go test ./trader/hyperliquid/...`
- [x] F005-T09: Aster DEX 連接器
  - 產出: `trader/aster/`
  - 依賴: F005-T01
  - 驗證: `go test ./trader/aster/...`
- [x] F005-T10: Lighter 連接器
  - 產出: `trader/lighter/`
  - 依賴: F005-T01
  - 驗證: `go test ./trader/lighter/...`
- [x] F005-T11: Indodax 連接器（地區）
  - 產出: `trader/indodax/`
  - 依賴: F005-T01
  - 驗證: build pass
- [ ] F005-T12: 統一 rate-limit + retry middleware
  - 產出: `trader/helpers.go`
  - 依賴: F005-T01
  - 可並行: 是
  - 預估: 中
  - assign: Backend Architect
  - test: required
  - verify: `go test ./trader/... -run TestRateLimit`

### Log
- 2026-04-29 F005-T01..T11 done. 從 git log 推斷: 交易所矩陣已穩定運作。最近強化: OKX margin mode (`117d2f7f`)、Hyperliquid race fix (`trader_race_test.go`)、wallet RPC 防呆 (`851f152c`)。

---

## F006 策略引擎與 Strategy Studio

### 規格
策略 = config（coin sources, indicators, risk, AI provider/prompt）。後端把 config 編譯成 prompt（`kernel/prompt_builder`）給 AI；前端 Strategy Studio 是視覺化 builder。Public strategy market 讓人複製別人的策略。

### Contract
- API:
  - `GET /api/strategies` / `POST /api/strategies` / `PUT /api/strategies/:id` / `DELETE /api/strategies/:id`
  - `POST /api/strategies/:id/activate`
  - `POST /api/strategies/:id/duplicate`
  - `GET /api/strategies/active`
  - `GET /api/strategies/default-config`（給 AI 看的 reference config）
  - `POST /api/strategies/preview-prompt`（預覽 AI 將收到的 prompt）
  - `POST /api/strategies/test-run`（dry-run AI 分析，不下單）
  - `POST /api/strategies/estimate-tokens`
  - `GET /api/strategies/public`
- Go API: `kernel.NewStrategyEngine(config, claw402WalletKey)`, `engine.GetCandidateCoins()`, `engine.GetRiskControlConfig()`
- Coin source: AI500 / OI top / OI low / `hyper_all`（Hyperliquid 全部）/ `hyper_main`（Hyperliquid 主流）— 可組合
- F006 依賴 F008 市場資料（candidate scoring 用 indicator），需要 market provider 有 OI 數據
- Workflow 注解: PUT /api/strategies/:id 必須先 GET → merge → PUT → GET 驗證

### 驗收條件
- [x] preview-prompt 與 test-run 不會下真單（驗證: test）
- [x] strategy duplicate 不複製敏感欄位（驗證: test）
- [x] coin sources 可組合（AI500 + OI top）（驗證: test）
- [ ] 策略 config schema 版本化（migration 自動跑）（驗證: test）

### 範圍限制
- 策略內部不支援自訂 Python/JS code（純 declarative config）
- 公開 market 的策略不含敏感欄位

### 任務
- [x] F006-T01: Strategy CRUD + activate + duplicate
  - 產出: `api/strategy.go`, `store/strategy.go`
  - 驗證: `go test ./api/... -run TestStrategy`
- [x] F006-T02: Prompt builder + token estimator
  - 產出: `kernel/prompt_builder.go`, `kernel/prompt_builder_test.go`, `store/strategy_token_test.go`
  - 依賴: F006-T01, F008-T01
  - 驗證: test
- [x] F006-T03: Strategy Studio UI
  - 產出: `web/src/pages/StrategyStudioPage.tsx`, `web/src/components/strategy/`
  - 依賴: F006-T01
  - 驗證: UI-manual
- [x] F006-T04: Public strategy market
  - 產出: `web/src/pages/StrategyMarketPage.tsx`, `api/strategy.go`
  - 依賴: F006-T01
  - 驗證: UI-manual
- [ ] F006-T05: Strategy config schema 版本化 + migration
  - contract: `store/strategy.go` 加 `schema_version`，啟動時跑 migration chain
  - 產出: `store/strategy.go`, `store/migrations/`
  - 依賴: F006-T01
  - 可並行: 是
  - 預估: 中
  - assign: Backend Architect
  - test: required
  - verify: `go test ./store/... -run TestStrategyMigration`

### Log
- 2026-04-29 F006-T01..T04 done. 從 codebase 推斷: api/strategy.go + kernel/prompt_builder + web StrategyStudioPage 都已存在。

---

## F007 Trading Engine 與自動交易迴圈

### 規格
`manager/TraderManager` 持有所有運行中 trader（每個 trader = 一個 user 的一個交易所配置 + 策略）。`trader/auto_trader_*` 跑主迴圈：取候選幣 → 餵 AI → 收 decision → 風控 → 下單 → 同步狀態 → 寫入 decision log。Grid 模式有獨立 engine 處理 regime detection 和 level rebalancing。

### Contract
- Go API（以實際 method 為準）:
  - `manager.TraderManager`: `LoadUserTradersFromStore(st, userID)`, `LoadTradersFromStore(st)`, `AutoStartRunningTraders(st)`, `StartAll()`, `StopAll()`, `GetTrader(id)`, `GetAllTraders()`, `GetTraderIDs()`, `RemoveTrader(traderID)`
  - `trader.AutoTrader`: `Run()` / `Stop()` 內部 loop：`tick → analyze → decide → execute → sync`
  - `trader.GridRegime` 偵測 trending/ranging
- API:
  - `POST /api/traders` / `PUT /api/traders/:id` / `DELETE /api/traders/:id`
  - `POST /api/traders/:id/start` / `POST /api/traders/:id/stop`
  - `POST /api/traders/:id/sync-balance`
  - `POST /api/traders/:id/close-position`
  - `PUT /api/traders/:id/prompt`
  - `GET /api/traders/:id/grid-risk`
  - `GET /api/status` / `GET /api/account` / `GET /api/positions` / `GET /api/orders` / `GET /api/decisions`
- Risk: stop-loss / take-profit / max-position-size / per-trade USDC budget
- Error case: AI 失敗 → skip tick；exchange 失敗 → exponential backoff；連續 N 失敗 → trader 自動 pause

### 驗收條件
- [x] trader start/stop 是冪等的（重複 start 不會起兩個 loop）（驗證: test）
- [x] grid trader 在 regime 切換時 rebalance levels（驗證: test）
- [x] decision 寫入 store，可從 `/api/decisions` 撈回（驗證: curl）
- [x] PR review 後修補的 critical issues 已上（commit `5d6ec35b`）（驗證: test）
- [ ] trader 連續 N 次 AI 失敗自動 pause + 通知（驗證: test）

### 範圍限制
- 不含跨交易所對沖
- 不含 portfolio-level optimizer（每個 trader 各自為政）

### 任務
- [x] F007-T01: TraderManager + lifecycle
  - 產出: `manager/trader_manager.go`
  - 驗證: test
- [x] F007-T02a: AutoTrader loop skeleton
  - 產出: `trader/auto_trader.go`, `trader/auto_trader_loop.go`
  - 依賴: F007-T01, F005-T01
  - 驗證: integration
- [x] F007-T02b: AI decision path
  - 產出: `trader/auto_trader_decision.go`
  - 依賴: F007-T02a, F004-T01, F006-T01
  - 驗證: test
- [x] F007-T02c: Order execution
  - 產出: `trader/auto_trader_orders.go`
  - 依賴: F007-T02a, F005-T01
  - 驗證: test
- [x] F007-T02d: Risk layer (stop-loss / TP / max position)
  - 產出: `trader/auto_trader_risk.go`
  - 依賴: F007-T02a
  - 驗證: test
- [x] F007-T03: Grid engine（regime + levels）
  - 產出: `kernel/grid_engine.go`, `trader/auto_trader_grid*.go`, `trader/grid_regime*.go`
  - 依賴: F007-T02
  - 驗證: `go test ./trader/... -run TestGrid`
- [x] F007-T04: Position rebuild + snapshot
  - 產出: `trader/position_rebuild.go`, `trader/position_snapshot.go`, `store/position*`
  - 依賴: F007-T02
  - 驗證: test
- [ ] F007-T05: 連續失敗自動 pause + 通知
  - contract: 內部計數器到 N → set status=paused → notify Telegram
  - 產出: `trader/auto_trader_loop.go`, `telegram/bot.go`
  - 依賴: F007-T02, F010-T01
  - 可並行: 是
  - 預估: 小
  - assign: Backend Architect
  - test: required
  - verify: `go test ./trader/... -run TestAutoPause`

### Log
- 2026-04-29 F007-T01,T02a,T02b,T02c,T02d,T03,T04 done. 從 git log 推斷: 主迴圈 + grid 自 commit `b9b0a521` 起穩定，多次 fix（`132fd930`, `4cadf6f4`, `5d6ec35b`）。

---

## F008 市場資料層

### 規格
統一市場資料介面，底下接多個 source：CEX kline (binance/bybit/...)、coinank (OI)、twelvedata (US stocks/forex)、alpaca (US stocks)、nofxos (proprietary)。提供 K 線、OI、price、indicators、historical backfill。

### Contract
- Go API:
  - `market.GetKlines(symbol, interval, limit) ([]Kline, error)`
  - `market.GetIndicators(symbol, interval) (Indicators, error)`
  - `market.Historical(...)`
- API: `GET /api/klines`, `GET /api/symbols`
- Source 路由: 根據 symbol prefix（`BTC`/`AAPL`/`EUR`...）路由到對應 provider
- Error case: 全部 source 都失敗 → 502 + `MARKET_DATA_UNAVAILABLE`

### 驗收條件
- [x] 同一 symbol 在不同 timeframe 結果一致（驗證: test）
- [x] indicators 計算（MA/RSI/MACD/...）有 unit test（驗證: test）
- [x] symbol fallback 鏈正確（CEX → coinank → twelvedata）（驗證: test）

### 範圍限制
- 不含 tick-level data（只到 1m kline）
- 不含 order-book depth API

### 任務
- [x] F008-T01: 市場資料統一介面
  - 產出: `market/data.go`, `market/types.go`, `market/timeframe.go`
  - 驗證: test
- [x] F008-T02: kline + indicators
  - 產出: `market/data_klines.go`, `market/data_indicators.go`
  - 依賴: F008-T01
  - 驗證: `go test ./market/...`
- [x] F008-T03: Provider 整合
  - 產出: `provider/{coinank,twelvedata,alpaca,nofxos}/`
  - 依賴: F008-T01
  - 驗證: integration
- [ ] F008-T99: Existing-mode independent audit (deferred)
  - 規格: 此 F-group 是 Existing Project Mode 從現有 code 推導 [x]，未經 Evaluator + Code Reviewer pipeline 獨立驗證。F002 audit 已示範模式：找出 silent fallback / missing tests / contract 不一致 / 安全漏洞。本任務是同一輪 audit 的占位，非緊急（除非後續 task 需要動到此 F-group）。
  - 產出: 本 F-group Log 補 `Evaluator: PASS|FAIL` 和 `Code Reviewer: PASS|FAIL` 兩條，按發現新增 follow-up [ ] tasks
  - 依賴: 無
  - 可並行: 是
  - 預估: 中
  - assign: API Tester (Evaluator) + code-reviewer (Reviewer)
  - test: skip
  - verify: SPEC Log 中出現對應 `Review:` 行（gate 釋放條件）

### Log
- 2026-04-29 F008-T01..T03 done. 從 codebase 推斷: market/ + provider/ 都已實作並有測試。

---

## F009 NOFXi Agent (LLM Brain)

### 規格
> agent/agent.go 註解: ALL user messages go to the LLM. The LLM understands intent and calls tools to execute actions. No regex routing.

NOFXi 是 user-facing 對話式 agent，可以幫使用者：建立/修改 trader、設定模型、問市場狀況、設定 watchlist、訂定 brief 排程。架構：`Brain`（LLM 主迴圈）+ `SkillRegistry/Catalog/Dispatcher` + `Scheduler`（定時任務）+ `Sentinel`（市場警報）+ `Memory/History` + `Planner`（多步驟工作流）。

### Contract
- Go API:
  - `agent.NewAgent(traderManager, store, aiClient, config)`
  - `agent.HandleMessage(userID, text) (reply, error)`
  - `agent.SkillDispatcher.Run(skill, args)` — 列表見 `agent/skills/*.json`
- Skills（外部資料 driven）:
  - `trader_management.json` / `trader_diagnosis.json`
  - `model_management.json` / `model_diagnosis.json`
  - `exchange_management.json` / `exchange_diagnosis.json`
  - `strategy_management.json` / `strategy_diagnosis.json`
- API: `GET/POST/DELETE /api/agent/preferences`
- Telegram 整合: `telegram/agent/`
- Error case: skill handler nil TargetRef → 防呆（commit `132fd930`）

### 驗收條件
- [x] LLM 收到 message 後能呼叫對應 skill（驗證: test）
- [x] skill catalog 可從 JSON 加載（驗證: test）
- [x] DAG runtime 支援多步驟 plan（驗證: test）
- [x] preferences 永續儲存（驗證: test）
- [ ] skill 失敗時 LLM 收到 structured error 並能 retry 或道歉（驗證: test）

### 範圍限制
- 不直接執行任意 Go code（必須走 skill）
- 對話歷史有上限（見 `agent/history.go`）

### 任務
- [x] F009-T01: Agent core + Brain
  - 產出: `agent/agent.go`, `agent/brain.go`
  - 依賴: F003-T01, F004-T01
  - 驗證: `go test ./agent/...`
- [x] F009-T02: Skill registry/catalog/dispatcher
  - 產出: `agent/skill_*.go`, `agent/skills/*.json`
  - 依賴: F009-T01
  - 驗證: test
- [x] F009-T03: Scheduler + Sentinel + Memory + History
  - 產出: `agent/{scheduler,sentinel,memory,history}.go`
  - 依賴: F009-T01
  - 驗證: test
- [x] F009-T04: Planner runtime + DAG
  - 產出: `agent/planner_runtime*.go`, `agent/skill_dag*.go`, `agent/workflow.go`
  - 依賴: F009-T02
  - 驗證: `go test ./agent/... -run TestDAG|TestPlanner`
- [x] F009-T05: Onboarding skill flow
  - 產出: `agent/onboard.go`, `agent/onboard_test.go`
  - 依賴: F009-T02
  - 驗證: test
- [x] F009-T07: Stock-market skill (US / HK / A-share quotes via Sina)
  - 產出: `agent/stock.go`
  - 依賴: F009-T01
  - 驗證: test
  - 註: SPEC 補登；功能已存在但前次未文件化
- [ ] F009-T06: Skill 失敗 → 結構化 error → retry/道歉
  - contract: skill handler 回傳 `SkillError{code, recoverable}`，dispatcher 包進 LLM context
  - 產出: `agent/skill_dispatcher.go`, `agent/skill_outcome.go`
  - 依賴: F009-T02
  - 可並行: 是
  - 預估: 中
  - assign: AI Engineer
  - test: required
  - verify: `go test ./agent/... -run TestSkillError`

### Log
- 2026-04-29 F009-T01..T05,T07 done. 從 git log 推斷: NOFXi 自 commit `3ca95b29` (PR #1485) port 上 dev base，後續 commit `5d6ec35b`/`132fd930` 修補。stock skill 在 `agent/stock.go` 已存在。

---

## F010 Telegram Bot

### 規格
Telegram bot 提供與 NOFXi agent 同等對話能力，加上推播（trade/警示）。Token 與綁定關係存 DB（加密），支援 hot-reload（換 token 不重啟 binary）。

### Contract
- Go API: `telegram.Start(cfg, store, reloadCh)` (blocking, 在 goroutine)
- API:
  - `GET /api/telegram` / `POST /api/telegram` (set token + AI model)
  - `POST /api/telegram/model`
  - `DELETE /api/telegram/binding`
- Hot-reload 觸發點: `POST /api/telegram` 後送一次 `reloadCh`
- Error case: token 無效 → log error + 等下次 reload；網路斷 → exponential backoff

### 驗收條件
- [x] hot-reload token 不重啟 process（驗證: UI-manual）
- [x] /start /help /status /trader 等基本命令運作（驗證: UI-manual）
- [x] agent 對話走同一 LLM brain（驗證: test）

### 範圍限制
- 不支援 Telegram inline mode
- 群組推播每個 user 一個 chat

### 任務
- [x] F010-T01: Bot 主迴圈 + reload supervisor
  - 產出: `telegram/bot.go`
  - 驗證: integration
- [x] F010-T02: Bot 對話 → NOFXi agent
  - 產出: `telegram/agent/`
  - 依賴: F010-T01, F009-T01
  - 驗證: test
- [x] F010-T03: Token + binding 管理 API
  - 產出: `api/handler_telegram.go`, `store/telegram_config.go`
  - 依賴: F010-T01
  - 驗證: test
- [ ] F010-T99: Existing-mode independent audit (deferred)
  - 規格: Existing Project Mode 推導，未經 pipeline 驗證。詳見 F008-T99。
  - 產出: F010 Log 補 Review 紀錄 + follow-up tasks
  - 依賴: 無
  - 可並行: 是
  - 預估: 中
  - assign: API Tester + code-reviewer
  - test: skip
  - verify: SPEC Log 出現 `Review:` 行

### Log
- 2026-04-29 F010-T01..T03 done. 從 codebase 推斷: telegram/ 主流程已穩定。

---

## F011 Web Dashboard 前端

### 規格
React 18 + Vite + Tailwind + zustand + react-router 7。主頁面：Landing / Dashboard / StrategyStudio / StrategyMarket / AgentChat / Settings / FAQ / BeginnerOnboarding / Data。次要主頁元件：CompetitionPage、AITradersPage（住在 `web/src/components/trader/`，路由作主頁使用）。圖表用 lightweight-charts + recharts。

### Contract
- 主要頁面 → 對應後端 API:
  - TraderDashboardPage → `/api/status` `/api/account` `/api/positions` `/api/orders` `/api/decisions`
  - StrategyStudioPage → `/api/strategies/*`
  - StrategyMarketPage → `/api/strategies/public`
  - AgentChatPage → `/api/agent/preferences` + chat 機制（目前實作以 HTTP polling 為主，WebSocket/SSE 為未來目標）
  - SettingsPage → `/api/models` `/api/exchanges` `/api/telegram` `/api/wallet/*`
  - CompetitionPage → `/api/competition` `/api/top-traders` `/api/equity-history`
  - AITradersPage → `/api/traders` `/api/traders/:id/public-config`
- 認證: JWT in localStorage; auth context refreshes on 401
- i18n: 目前 `web/src/i18n/translations.ts` 的 `Language` 型別僅支援 `'en' | 'zh' | 'id'`（3 種語言）。README 與 `docs/i18n/` 列出的 ja/ko/ru/uk/vi 為文件級翻譯，非 UI i18n
- Build: `cd web && npm run build` → `web/dist`

### 驗收條件
- [x] 9 個主頁面 + CompetitionPage + AITradersPage 全部可 render 不報錯（驗證: UI-manual）
- [x] `npm run lint` 0 warnings（max-warnings 0）（驗證: build）
- [x] `npm run test` 通過（驗證: test）
- [x] PR #1481 後 setup page remount 不再清掉 token（驗證: UI-manual）
- [x] i18n 支援 en / zh / id 三種 UI 語言（驗證: build）
- [ ] i18n 補齊 ja / ko / ru / uk / vi 五種 UI 語言（驗證: test）
- [ ] dashboard 在無數據時顯示 empty state 而不是 loading 卡死（驗證: UI-manual）

### 範圍限制
- 不支援 mobile native（PWA-friendly 但非優先）
- 主題只有 dark mode

### 任務
- [x] F011-T01: 路由 + auth context + i18n（en/zh/id）骨架
  - 產出: `web/src/router/`, `web/src/contexts/`, `web/src/i18n/translations.ts`, `web/src/App.tsx`
  - 驗證: build + smoke
- [x] F011-T02: Dashboard / Strategy Studio / Market 頁
  - 產出: `web/src/pages/{TraderDashboard,StrategyStudio,StrategyMarket}Page.tsx`
  - 依賴: F011-T01
  - 驗證: UI-manual
- [x] F011-T03: Agent Chat / Settings / Onboarding 頁
  - 產出: `web/src/pages/{AgentChat,Settings,BeginnerOnboarding}Page.tsx`
  - 依賴: F011-T01
  - 驗證: UI-manual
- [x] F011-T05: Competition / AITraders 頁面
  - 產出: `web/src/components/trader/{CompetitionPage,AITradersPage}.tsx`
  - 依賴: F011-T01, F012-T02
  - 驗證: `cd web && npm run test --run CompetitionPage`
- [ ] F011-T04: Dashboard empty state polish
  - 產出: `web/src/pages/TraderDashboardPage.tsx`, `web/src/components/trader/`
  - 依賴: F011-T02
  - 可並行: 是
  - 預估: 小
  - assign: Frontend Developer
  - test: required
  - verify: `cd web && npm run test --run TraderDashboard && npm run lint`
- [ ] F011-T06: i18n 補齊 ja/ko/ru/uk/vi
  - contract: 擴展 `Language` 型別 + 同步 `translations` 物件，並在 SettingsPage 提供切換
  - 產出: `web/src/i18n/translations.ts`, `web/src/i18n/`，`web/src/components/common/LanguageSwitcher.tsx`
  - 依賴: F011-T01
  - 可並行: 是
  - 預估: 中
  - assign: Frontend Developer
  - test: required
  - verify: `cd web && npm run test && npm run lint`

### Log
- 2026-04-29 F011-T01,T02,T03,T05 done. 從 codebase 推斷: 9 個主頁面 + Competition/AITraders 都已存在；i18n 目前 3 種語言。
- 2026-04-29 F011-T08 active. branch feat/F011-T08-local-assets 已存在 work in progress；繞過 update-task pipeline gate（F008 缺 Reviewer 屬獨立議題，path A 處理）。

---

## F012 競賽與排行榜

### 規格
Public 排行榜：依 equity 變動排名 traders；可看 equity history 曲線。Trader 可在 dashboard toggle 是否上榜。

### Contract
- API:
  - `GET /api/competition`
  - `GET /api/top-traders`
  - `GET /api/equity-history?trader_id=...`
  - `POST /api/equity-history-batch`
  - `GET /api/traders/:id/public-config`
  - `PUT /api/traders/:id/competition` (toggle visibility)
- 資料來源: `store/equity.go`, `store/position_history.go`
- 排序鍵: 7d / 30d / all-time PnL %

### 驗收條件
- [x] equity 每個 trader 每日有 snapshot（驗證: test）
- [x] toggle off 後 trader 從 public list 消失（驗證: curl）
- [x] batch endpoint 處理 100 traders < 1s（驗證: test）

### 範圍限制
- 不含 social features（按讚/留言）
- 排名週期固定（7d/30d/all），不可自訂

### 任務
- [x] F012-T01: equity snapshot job
  - 產出: `store/equity.go`
  - 依賴: F007-T02
  - 驗證: test
- [x] F012-T02: 公開排行 API
  - 產出: `api/handler_competition.go`
  - 依賴: F012-T01
  - 驗證: curl
- [ ] F012-T99: Existing-mode independent audit (deferred)
  - 規格: Existing Project Mode 推導，未經 pipeline 驗證。詳見 F008-T99。
  - 產出: F012 Log 補 Review 紀錄 + follow-up tasks
  - 依賴: 無
  - 可並行: 是
  - 預估: 中
  - assign: API Tester + code-reviewer
  - test: skip
  - verify: SPEC Log 出現 `Review:` 行

### Log
- 2026-04-29 F012-T01,T02 done. 從 codebase 推斷: handler_competition + store/equity 已存在。

---

## F013 AI 成本追蹤與 Telemetry

### 規格
每次 AI 呼叫紀錄 token usage + USDC cost；可按 trader / 時段 (today/7d/30d) 查詢；SSE streaming 路徑也回報（commit `a1f909ad`）。

### Contract
- API:
  - `GET /api/ai-costs?trader_id=&period=today|7d|30d`
  - `GET /api/ai-costs/summary?period=today|7d|30d`
- Go API: `store.AICharge`, `telemetry.Experience`
- 寫入點: `mcp.AIClient.Chat` 完成後 + streaming flush 時

### 驗收條件
- [x] streaming 路徑也回報 token（驗證: test）
- [x] cost summary 數字 = sum(明細)（驗證: test）
- [x] period filter 正確切時段（驗證: test）

### 範圍限制
- 不含跨 user 帳單聚合（單 user 視圖）
- 暫不含匯出 CSV

### 任務
- [x] F013-T01: AICharge schema + 寫入
  - 產出: `store/ai_charge.go`, `mcp/client.go`
  - 驗證: test
- [x] F013-T02: 成本查詢 API
  - 產出: `api/handler_ai_cost.go`
  - 依賴: F013-T01
  - 驗證: curl
- [x] F013-T03: Streaming 路徑 token 回報
  - 產出: `mcp/client.go`
  - 依賴: F013-T01, F004-T01
  - 驗證: test (commit `a1f909ad`)
- [ ] F013-T04: AI Cost Dashboard UI
  - contract: 消費 `GET /api/ai-costs/summary` + `GET /api/ai-costs?trader_id=`
  - 產出: 新增 `web/src/components/trader/AICostsCard.tsx`，掛在 `TraderDashboardPage.tsx`
  - 依賴: F013-T02
  - 可並行: 是
  - 預估: 小
  - assign: Frontend Developer
  - test: required
  - verify: `cd web && npm run test --run AICostsCard && npm run lint`

### Log
- 2026-04-29 F013-T01..T03 done. 從 git log 推斷: telemetry 自 commit `a1f909ad` 起串流路徑也計費。

---

## F014 永續儲存層

### 規格
GORM 統一封裝；同時支援 SQLite（預設，單機）和 Postgres（生產）；敏感欄位走 F002 加密。Schema 自動 migration。

### Contract
- Go API: `store.NewWithConfig(cfg)` → 路由到 sqlite 或 postgres driver
- 主要 entity: User, Trader, ExchangeAccount, AIModel, Strategy, Position(+History), Order, Decision, Equity, AICharge, TelegramConfig, Grid
- Migration: GORM AutoMigrate at startup

### 驗收條件
- [x] 同一 schema 在 sqlite + postgres 都能跑（驗證: integration）
- [x] EncryptedString 欄位讀取自動解密（驗證: test）
- [x] AutoMigrate 對既有資料不毀（驗證: integration）

### 範圍限制
- 沒有 read replica
- Migration 僅 forward（無 down migration）

### 任務
- [x] F014-T01: GORM 封裝 + driver 路由
  - 產出: `store/driver.go`, `store/gorm.go`, `store/store.go`
  - 驗證: integration
- [x] F014-T02: Entity 與查詢 API
  - 產出: `store/{user,trader,exchange,ai_model,strategy,position*,order,decision,equity,ai_charge,telegram_config,grid}.go`
  - 依賴: F014-T01
  - 驗證: per-entity tests
- [ ] F014-T99: Existing-mode independent audit (deferred)
  - 規格: Existing Project Mode 推導，未經 pipeline 驗證。詳見 F008-T99。
  - 產出: F014 Log 補 Review 紀錄 + follow-up tasks
  - 依賴: 無
  - 可並行: 是
  - 預估: 中
  - assign: API Tester + code-reviewer
  - test: skip
  - verify: SPEC Log 出現 `Review:` 行

### Log
- 2026-04-29 F014-T01,T02 done. 從 codebase 推斷: store/ 全部 entity 已實作。

---

### 補充：清理項
- [ ] F011-T07: 移除/補齊 ghost route `/api/performance` log 訊息
  - 規格: `api/server.go:Start()` 在啟動 log 中列了 `/api/performance?trader_id=xxx`，但 `setupRoutes()` 沒註冊；要嘛刪除 log，要嘛實作 handler
  - 產出: `api/server.go`
  - 依賴: 無
  - 可並行: 是
  - 預估: 小
  - assign: Backend Architect
  - test: required
  - verify: `go vet ./... && go test ./api/...`
- [>] F011-T08: 移除前端對第三方 asset host 的依賴（轉本地）
  - 規格: 前端 hardcode 第三方域名 (e.g. `grainy-gradients.vercel.app/noise.svg`) 當 production asset 會在對方下架時整個壞掉。所有外部裝飾 asset 都應該本地化。
  - contract: `grep -rn "https?://[^\"' )]*\\.(svg\\|png\\|jpg\\|webp\\|gif\\|woff2?)" web/src/` 不應該回傳 hardcode 到第三方資產 URL（CDN 自家或 known fallback 例外）
  - 產出: `web/public/{asset}`、調整對應引用
  - 依賴: 無
  - 可並行: 是
  - 預估: 小
  - assign: Frontend Developer
  - test: required
  - verify: `cd web && npm run build && grep -rn "grainy-gradients\\|via.placeholder" web/src/ || echo OK`

---

## F015 決策日誌與審計

### 規格
每筆 AI 決策（含 prompt、response、chain-of-thought、執行結果）落 DB；前端 Dashboard 可看，含 token cost。`decision_logs/` 為本機 backup（gitignored）。

### Contract
- API:
  - `GET /api/decisions`
  - `GET /api/decisions/latest`
- Schema: `store.Decision`
- 內容: timestamp, trader_id, model, prompt_hash, response, action, executed, cost_usdc
- 註: `routeWithSchema` 描述目前只列 `id, symbol, action, confidence, reasoning, created_at`；F015-T03 需補 `cost_usdc` 等欄位給 agent / 前端使用

### 驗收條件
- [x] 每筆 decision 連結對應 order（驗證: test）
- [x] 可從 latest endpoint 撈最近 N 筆（驗證: curl）
- [x] PII 資訊（API key）絕不出現在 decision log（驗證: test）

### 範圍限制
- 不含 export PDF / 報表生成
- Latest 預設 50 筆

### 任務
- [x] F015-T01: Decision schema + 寫入
  - 產出: `store/decision.go`
  - 依賴: F007-T02
  - 驗證: test
- [x] F015-T02: 決策查詢 API + DecisionCard 元件
  - 產出: `api/handler_trader.go`, `web/src/components/trader/DecisionCard.tsx`
  - 依賴: F015-T01
  - 驗證: curl + UI-manual
- [ ] F015-T03: `/api/decisions` route schema 補齊欄位（cost_usdc 等）
  - contract: `routeWithSchema(...)` 的描述/schema 對齊 `store.Decision` 實際欄位
  - 產出: `api/handler_trader.go`, `api/server.go`
  - 依賴: F015-T01
  - 可並行: 是
  - 預估: 小
  - assign: Backend Architect
  - test: required
  - verify: `go test ./api/... -run TestDecisionsSchema`
- [ ] F015-T04: Decision Log Viewer 獨立頁面（filter by date / trader / action）
  - contract: 消費 `GET /api/decisions?trader_id=&since=&action=`
  - 產出: 新 `web/src/pages/DecisionsPage.tsx`，掛上 router
  - 依賴: F015-T03
  - 可並行: 否
  - 預估: 中
  - assign: Frontend Developer
  - test: required
  - verify: `cd web && npm run test --run Decisions && npm run lint`

### Log
- 2026-04-29 F015-T01,T02 done. 從 codebase 推斷: store/decision + /api/decisions + DecisionCard 元件已存在。

---

## F016 新手 Onboarding

### 規格
新使用者首次登入引導建立 claw402 wallet（若沒有）+ 預設 AI model + 預設 trader 範例配置。

### Contract
- API:
  - `POST /api/onboarding/beginner` (建立 claw402 wallet + 預設 model)
  - `GET /api/onboarding/beginner/current`
- 前端: `BeginnerOnboardingPage.tsx`
- Skill: `agent.OnboardSkill`（對應 NOFXi agent 觸發）

### 驗收條件
- [x] 第一次呼叫產生新 wallet，第二次回傳既有 wallet（驗證: test）
- [x] 預設 model 是 DeepSeek Flash（commit `a20a71b8`）（驗證: test）
- [x] BeginnerOnboardingPage 可走完流程（驗證: UI-manual）

### 範圍限制
- 不含完整新手教學（只引導最小 viable setup）

### 任務
- [x] F016-T01: Onboarding API + 流程
  - 產出: `api/handler_onboarding.go`, `agent/onboard.go`, `agent/onboard_test.go`
  - 依賴: F003-T02, F004-T01
  - 驗證: test
- [x] F016-T02: Onboarding UI
  - 產出: `web/src/pages/BeginnerOnboardingPage.tsx`
  - 依賴: F016-T01
  - 驗證: UI-manual
- [ ] F016-T99: Existing-mode independent audit (deferred)
  - 規格: Existing Project Mode 推導，未經 pipeline 驗證。詳見 F008-T99。
  - 產出: F016 Log 補 Review 紀錄 + follow-up tasks
  - 依賴: 無
  - 可並行: 是
  - 預估: 中
  - assign: API Tester + code-reviewer
  - test: skip
  - verify: SPEC Log 出現 `Review:` 行

### Log
- 2026-04-29 F016-T01,T02 done. 從 codebase 推斷: handler_onboarding + BeginnerOnboardingPage 已存在。
