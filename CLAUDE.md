# CLAUDE.md

## 產品上下文

- **目標用戶**：個人 crypto/股票交易者，想用 AI 自動跑策略但不想花時間管 API key、配額、prompt、模型路由
- **核心痛點**：傳統 AI trading 工具要手動接 OpenAI key、ccxt key、prompt 模板，斷一個就停。NOFX 用 USDC wallet + 內建 8 provider × 10 exchange，拿到一鍵安裝即可跑
- **使用場景**：
  1. 新手裝完跑 `/onboarding/beginner` → 自動拿 Claw402 wallet → 預設 DeepSeek Flash → 範本策略 → 連 Binance demo → 開跑
  2. 進階用戶在 Strategy Studio 拖 coin source/indicators/risk → 多策略並行 → 看 leaderboard 比績效
  3. 透過 Telegram 對 NOFXi agent 說「BTC 過 70k 通知我」「把 BTC trader 切去 OKX」
- **規模預期**：單機 binary，預期 < 1k traders / process；多 user → Postgres + 反向代理
- **差異化**：
  - x402 USDC wallet-first（無 API key flow）
  - LLM-as-router NOFXi agent（vs 規則型 bot）
  - 10 個交易所 + 8 個 AI provider 同一介面
  - 自托管、開源（AGPL-3.0）

## Common Commands

### Backend (Go)
```bash
make run                     # go run main.go
make build                   # go build -o nofx
make test                    # 全部測試（go test ./... + web vitest）
make test-backend            # 只跑 go test
make test-coverage           # 產 coverage.html
make fmt                     # go fmt ./...
make lint                    # golangci-lint run
go test ./{pkg}/...          # 單個 package
```

### Frontend (web/)
```bash
cd web && npm install
cd web && npm run dev        # vite dev server
cd web && npm run build      # tsc + vite build → web/dist
cd web && npm run lint       # eslint，max-warnings 0
cd web && npm run test       # vitest run
cd web && npm run format     # prettier write
```

### Docker
```bash
make docker-build            # docker compose build
make docker-up               # docker compose up -d
make docker-logs             # follow logs
make docker-down
```

### 知識圖譜
```bash
graphify: python3 -m graphify . && graphify hook install   # 首批 code 完成後執行
/graphify query <symbol>     # 查 import 關係
/graphify path A B           # 兩 symbol 之間的依賴鏈
```

### Smoke checks（驗證 task done 用）
```bash
curl -fsS localhost:8080/api/health | grep -q ok          # backend alive
curl -fsS localhost:3000 | head -c 200 | grep -qi nofx    # frontend serves
go build ./... && echo OK                                  # backend compiles
cd web && npm run build && echo OK                         # frontend compiles
```

## 累積的規則

### 安全
- 任何 secret-like 欄位（含 `key`, `secret`, `token`, `password`, `privkey`）寫 DB 必須用 `crypto.EncryptedString`
- `.env`、`config.json`、`*.key`、`*.pem`、`secrets/` 全部 gitignored；commit 前用 `git diff --cached` 自查
- API 回應絕不洩漏 stack trace，error 用 `api/errors.go` 的 sanitized 格式

### 後端
- 新增 API endpoint 必須走 `s.route()` 或 `s.routeWithSchema()`（保留 description 給前端 / agent 看）
- 新交易所實作 `trader/types/Trader` interface；不在 `auto_trader_*.go` 寫交易所專屬分支
- 新 AI provider 實作 `mcp.AIClient`，並走 x402 路徑（API-key 模式作為 fallback）
- thinking model 在 provider adapter 層 strip 掉 `max_tokens`
- DB schema 變更先在 sqlite 跑通，再用 `DB_TYPE=postgres` 跑 integration

### 前端
- `npm run lint` 設 `max-warnings 0`：lint warning 視同 fail
- 新元件用 Tailwind class，不寫 inline style
- API client 用 `web/src/lib/`（不直接 `fetch` / `axios`）
- 表單敏感欄位走 transport encryption helper（看 `crypto/public-key` 設定）

### Agent / Skill
- 新 skill：在 `agent/skills/*.json` 加 schema → 在 `agent/skill_execution_handlers.go` 加 handler；不要在 `brain.go` 加 if/else
- Skill handler 取 `TargetRef` 前必須 nil check（commit `132fd930` 教訓）
- Skill 失敗回 structured error，由 dispatcher 包進 LLM context

### 測試
- 動到金額/auth/wallet/簽章 → unit test 必寫
- Bug fix 先寫 regression test 再修
- 跨 sqlite/postgres 行為差異 → 兩個 driver 都跑
- E2E：`trader/binance/sync_e2e_test.go` 是模板
