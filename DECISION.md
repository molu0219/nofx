# DECISION.md

> session 開始讀一次。compact 或結束時寫入。

## 核心守則

1. **Wallet-first，不是 API-key-first**：所有新 AI provider 整合都要先支援 x402/USDC 流程；API key 是 fallback。預設 gateway 為 Claw402（Base USDC）。
2. **敏感欄位一律走 `crypto.EncryptedString`**：API key、wallet privkey、Telegram token、AI model key 等寫 DB 前必經 AES-256-GCM。Plaintext 絕不落盤、不打 log。
3. **Trader interface 是契約**：新增交易所 → 實作 `trader/types/Trader`（必要時 `GridTrader`），不在主迴圈裡寫交易所特例分支。CEX/DEX 共用同一介面。
4. **Skill-driven Agent**：NOFXi 對 user 的所有動作必須走 skill registry。LLM 不直接執行業務邏輯，skill schema 從 `agent/skills/*.json` 加載。
5. **Streaming 必計費**：任何 AI 路徑（含 SSE/stream）完成時必須回報 token 給 telemetry，否則 cost 數字會漂。
6. **Thinking model 不傳 `max_tokens`**：DeepSeek/Claude thinking 等 reasoning model 觸發 max_tokens 會 400；client adapter 必須在 provider 層 strip。
7. **402 重試不重簽**：x402 retry 路徑使用同一 nonce/payment，避免 double charge。Retries: max 5，base wait 3s，timeout 5min（涵蓋長推理）。
8. **DB schema 同時跑 sqlite + postgres**：所有 store 變更都要在兩種 driver 下測試；任何 PG-only 語法（如 JSONB）必須有 sqlite fallback。

---

## 跨功能

### D001: 預設 AI provider gateway 用 Claw402
CHOSE: Claw402 > 自建 x402 > 直接 OpenAI/Anthropic API
WHY: 一個 wallet 通吃 15+ 模型，user 不用管 API key 配額，且 NOFX 拿 referral 收入
RISK: Claw402 是 single point；需要 fallback 到原生 provider API key
DATE: 2026-04-20
RATING: ★★★★

### D002: Strategy = declarative config，不是 code
CHOSE: JSON config + prompt builder > user-supplied Python/JS
WHY: 不用 sandbox，安全簡單；prompt 生成是 deterministic 可預覽 (`/strategies/preview-prompt`)
RISK: 表達能力受限；複雜策略需擴 schema
DATE: 2026-04-20
RATING: ★★★★

### D003: Trader manager 用單行程內 goroutine 而非 worker pool
CHOSE: per-trader goroutine + sync.Map > task queue / RPC workers
WHY: 簡單、低延遲；單機 binary 是核心定位
RISK: 達到 ~1k traders/process 就要分片；要監控 goroutine 洩漏
DATE: 2026-04-20
RATING: ★★★

### D004: Agent 全部走 LLM，沒有 regex routing
CHOSE: LLM-as-router + skill catalog > rule-based intent classifier
WHY: 對話自然、易擴展（加 skill = 加 JSON）；註解見 `agent/agent.go`
RISK: 增加 LLM 成本與延遲；prompt injection 風險靠 skill 邊界限制
DATE: 2026-04-20
RATING: ★★★★

### D005: SQLite 預設、Postgres 是 production option
CHOSE: SQLite-first > Postgres-only
WHY: `curl install.sh | bash` 體驗：不用裝 DB；Postgres 留給多 user/HA 場景
RISK: 開發必須兩 driver 都測；某些 PG 專用功能不能用
DATE: 2026-04-20
RATING: ★★★★

### D006: 加密用 AES-256-GCM + RSA-2048
CHOSE: AES-GCM (server) + RSA (transport) > KMS / mTLS
WHY: 自托管定位，不依賴外部 KMS；env 載入足夠
RISK: 金鑰輪換需手動；env 洩漏 = 完全失守
DATE: 2026-04-20
RATING: ★★★

### D007: 前端用 zustand + SWR，不用 Redux Toolkit Query
CHOSE: zustand (state) + swr (server cache) > Redux/RTK
WHY: bundle 小、學習曲線低；專案規模還沒到 Redux 必要
RISK: 大量 cross-store 互動時手動串接成本上升
DATE: 2026-04-20
RATING: ★★★

### D008: `/api/crypto/decrypt` 暫時保持 unauth — 待 F002-T07 收斂
CHOSE: 暫不變動現況 (unauth endpoint) > 立即加 JWT > 立即拆除 endpoint
WHY: F002 Reviewer 標 HIGH 的 decryption oracle 風險真實存在，但現有前端 SetupPage / 註冊 / claw402 wallet 設定流程是否在 pre-login 階段呼叫此 endpoint 尚未 audit；貿然加 JWT 會破壞 onboarding。先靠 F002-T05 的 TS=0 reject 把 replay window 限縮到 5 分鐘，後續由 F002-T07 完整收斂（JWT / AAD purpose 驗證 / rate limit 三選一或合併）。
RISK: 5 分鐘 replay window 內仍可 oracle；需在 F002-T07 解決前不要把更多敏感欄位放進此 endpoint 的解密來源。
DATE: 2026-04-29
RATING: ★★★ (residual risk acknowledged, mitigation 已部分到位)
