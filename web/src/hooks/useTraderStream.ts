import { useEffect, useRef } from 'react'
import { mutate } from 'swr'

import type { AccountInfo, Position } from '../types'

// streamTickPayload mirrors api/handler_stream.go's streamTickPayload exactly.
// Both ends evolve together; if the backend shape changes, update this and
// rebuild the dashboard rather than coercing client-side.
interface StreamTickPayload {
  trader_id: string
  timestamp: string
  equity: number
  wallet_balance: number
  available_balance: number
  unrealized_pnl: number
  margin_used: number
  total_pnl_pct: number
  positions: Array<Record<string, unknown>>
}

interface UseTraderStreamOpts {
  /**
   * Disable the SSE connection. Useful when the dashboard is hidden, the
   * trader is paused, or you're debugging polling behaviour. Defaults to
   * false (stream is on whenever traderId is set).
   */
  disabled?: boolean
}

/**
 * useTraderStream subscribes to the SSE feed at /api/stream/trader/:id and
 * pushes updates into the SWR cache so the dashboard re-renders on every
 * upstream price tick (~150 ms in practice — bounded by Bybit's WS rate).
 *
 * The SWR keys mutated here MUST match what AppRoutes.tsx uses:
 *   account-<traderId>     →  AccountInfo
 *   positions-<traderId>   →  Position[]
 *
 * When the connection drops, SWR's existing 15-second poll keeps the dashboard
 * alive as a fallback. A bounded reconnect loop (1s, 2s, 5s, 10s, 30s) takes
 * care of transient blips without spamming the backend.
 */
export function useTraderStream(
  traderId: string | null | undefined,
  token: string | null | undefined,
  opts: UseTraderStreamOpts = {}
) {
  const reconnectAttemptsRef = useRef(0)
  const sourceRef = useRef<EventSource | null>(null)
  const closedRef = useRef(false)

  useEffect(() => {
    if (!traderId || !token || opts.disabled) {
      return
    }
    closedRef.current = false

    const open = () => {
      if (closedRef.current) return

      // EventSource doesn't support custom headers, so the JWT goes in a
      // query param. The backend's handleStreamTrader accepts both forms.
      const url = `/api/stream/trader/${encodeURIComponent(
        traderId
      )}?token=${encodeURIComponent(token)}`
      const es = new EventSource(url)
      sourceRef.current = es

      es.addEventListener('tick', (ev: MessageEvent) => {
        reconnectAttemptsRef.current = 0
        try {
          const payload = JSON.parse(ev.data) as StreamTickPayload
          // Patch the account cache: keep the existing shape and overwrite
          // the live numeric fields so other pre-computed fields (e.g.
          // initial_balance, lastUpdate) survive between ticks.
          mutate<AccountInfo>(
            `account-${traderId}`,
            (prev) => ({
              ...(prev || ({} as AccountInfo)),
              total_equity: payload.equity,
              available_balance: payload.available_balance,
              total_unrealized_profit: payload.unrealized_pnl,
              total_pnl_pct: payload.total_pnl_pct,
              total_wallet_balance: payload.wallet_balance,
              total_margin_used: payload.margin_used,
            }),
            { revalidate: false }
          )
          mutate<Position[]>(
            `positions-${traderId}`,
            (payload.positions as unknown as Position[]) || [],
            { revalidate: false }
          )
        } catch {
          // Malformed payload — ignore this tick; SWR poll catches up.
        }
      })

      es.addEventListener('close', () => {
        es.close()
      })

      es.onerror = () => {
        es.close()
        sourceRef.current = null
        if (closedRef.current) return
        // Bounded backoff so a flaky network doesn't hammer the API.
        const delays = [1000, 2000, 5000, 10000, 30000]
        const delay =
          delays[Math.min(reconnectAttemptsRef.current, delays.length - 1)]
        reconnectAttemptsRef.current += 1
        window.setTimeout(open, delay)
      }
    }

    open()

    return () => {
      closedRef.current = true
      sourceRef.current?.close()
      sourceRef.current = null
      reconnectAttemptsRef.current = 0
    }
  }, [traderId, token, opts.disabled])
}
