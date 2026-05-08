// DecisionOutcomesPanel — diagnostic view that surfaces the per-decision
// PnL granularity the user asked for (vs aggregate ROI). Pulls the
// /api/traders/:id/decision-outcomes endpoint and renders one row per
// decision, plus a compressed strategy-version timeline at the top so the
// reader can correlate "config changed at v7" with "PnL flipped here".
//
// Self-contained: drop into any page that has a traderId. Uses SWR so
// repeated mounts share cache; refresh interval defaults to 30s (cheap;
// the underlying join is per-trader on indexed columns).
import { useState } from 'react'
import useSWR from 'swr'
import { api } from '../../lib/api'
import type { DecisionOutcome, StrategyVersion } from '../../types'
import { Loader2, RefreshCw, History } from 'lucide-react'
import { t, type Language } from '../../i18n/translations'

interface Props {
  traderId: string
  language: Language
}

export function DecisionOutcomesPanel({ traderId, language }: Props) {
  const [limit, setLimit] = useState(50)

  const {
    data: outcomes,
    isLoading: outcomesLoading,
    mutate: refreshOutcomes,
  } = useSWR<DecisionOutcome[]>(
    traderId ? `decision-outcomes-${traderId}-${limit}` : null,
    () => api.getDecisionOutcomes(traderId, limit),
    { refreshInterval: 30000, revalidateOnFocus: false }
  )

  const { data: versions } = useSWR<StrategyVersion[]>(
    traderId ? `strategy-versions-${traderId}` : null,
    () => api.getStrategyVersions(traderId, 20),
    { refreshInterval: 60000, revalidateOnFocus: false }
  )

  if (!traderId) return null

  // Compute running PnL so the user sees cumulative drift across decisions
  // — a single decision's PnL number is noise; the trajectory is signal.
  // Outcomes come newest-first; we reduce-iterate forward-in-time, then
  // flip back for display. Using reduce keeps the closure pure (no
  // post-render mutation) which the lint rules want.
  const orderedAsc = (outcomes ?? []).slice().reverse()
  const enriched = orderedAsc.reduce<
    Array<DecisionOutcome & { cumulative_pnl: number; is_closed: boolean }>
  >((acc, o) => {
    const isClosed = o.position_status === 'CLOSED'
    const prev = acc.length > 0 ? acc[acc.length - 1].cumulative_pnl : 0
    const cumulative = isClosed ? prev + o.realized_pnl : prev
    acc.push({ ...o, cumulative_pnl: cumulative, is_closed: isClosed })
    return acc
  }, [])
  const display = enriched.slice().reverse()
  const finalCumulative =
    enriched.length > 0 ? enriched[enriched.length - 1].cumulative_pnl : 0

  const sources = (versions ?? []).map((v) => v.change_source)
  const summary = {
    versions: versions?.length ?? 0,
    optimizerChanges: sources.filter((s) => s === 'optimizer').length,
    userChanges: sources.filter((s) => s === 'user').length,
    decisions: outcomes?.length ?? 0,
    closedTrades: enriched.filter((o) => o.is_closed).length,
    netPnL: finalCumulative,
  }

  return (
    <div className="rounded-xl nofx-glass border border-nofx-gold/20 p-5 space-y-4">
      {/* Header — quick summary + refresh */}
      <div className="flex items-center justify-between">
        <div className="flex items-center gap-2">
          <History className="w-4 h-4 text-nofx-gold" />
          <h3 className="text-sm font-semibold text-nofx-text">
            {t('decisionOutcomes.title', language)}
          </h3>
          <span className="text-xs text-nofx-text-muted">
            {summary.decisions} {t('decisionOutcomes.decisions', language)}
            {' / '}
            {summary.closedTrades} {t('decisionOutcomes.closed', language)}
            {' · '}
            <span
              className={
                summary.netPnL >= 0 ? 'text-nofx-green' : 'text-red-400'
              }
            >
              {summary.netPnL >= 0 ? '+' : ''}
              {summary.netPnL.toFixed(2)} USDT
            </span>
          </span>
        </div>
        <button
          type="button"
          onClick={() => refreshOutcomes()}
          className="p-1 rounded hover:bg-white/10 transition-colors"
          title={t('decisionOutcomes.refresh', language)}
        >
          {outcomesLoading ? (
            <Loader2 className="w-4 h-4 animate-spin text-nofx-text-muted" />
          ) : (
            <RefreshCw className="w-4 h-4 text-nofx-text-muted" />
          )}
        </button>
      </div>

      {/* Strategy version timeline — compressed strip */}
      {versions && versions.length > 0 && (
        <div>
          <div className="text-xs text-nofx-text-muted mb-2">
            {t('decisionOutcomes.versionTimeline', language)} (
            {summary.versions} {t('decisionOutcomes.versions', language)},{' '}
            {summary.optimizerChanges}{' '}
            {t('decisionOutcomes.byOptimizer', language)}, {summary.userChanges}{' '}
            {t('decisionOutcomes.byUser', language)})
          </div>
          <div className="flex flex-wrap gap-1">
            {versions.map((v) => (
              <div
                key={v.id}
                title={`v${v.version_num} (${v.change_source}) at ${new Date(
                  v.created_at
                ).toLocaleString()}\n${v.reasoning || ''}`}
                className="text-[10px] px-1.5 py-0.5 rounded font-mono cursor-help"
                style={{
                  background:
                    v.change_source === 'optimizer'
                      ? 'rgba(168, 85, 247, 0.15)'
                      : v.change_source === 'user'
                        ? 'rgba(34, 197, 94, 0.15)'
                        : 'rgba(148, 163, 184, 0.15)',
                  color:
                    v.change_source === 'optimizer'
                      ? '#a855f7'
                      : v.change_source === 'user'
                        ? '#22c55e'
                        : '#94a3b8',
                  border: '1px solid currentColor',
                }}
              >
                v{v.version_num}
              </div>
            ))}
          </div>
        </div>
      )}

      {/* Decision rows */}
      <div className="overflow-x-auto">
        <table className="w-full text-xs">
          <thead className="text-[10px] uppercase tracking-wide text-nofx-text-muted">
            <tr className="border-b border-white/5">
              <th className="text-left py-2 font-medium">
                {t('decisionOutcomes.cycle', language)}
              </th>
              <th className="text-left py-2 font-medium">
                {t('decisionOutcomes.time', language)}
              </th>
              <th className="text-left py-2 font-medium">v</th>
              <th className="text-left py-2 font-medium">
                {t('decisionOutcomes.symbol', language)}
              </th>
              <th className="text-right py-2 font-medium">
                {t('decisionOutcomes.entry', language)}
              </th>
              <th className="text-right py-2 font-medium">
                {t('decisionOutcomes.exit', language)}
              </th>
              <th className="text-right py-2 font-medium">PnL</th>
              <th className="text-right py-2 font-medium">Σ PnL</th>
              <th className="text-left py-2 font-medium">
                {t('decisionOutcomes.status', language)}
              </th>
            </tr>
          </thead>
          <tbody className="font-mono">
            {display.length === 0 && !outcomesLoading && (
              <tr>
                <td
                  colSpan={9}
                  className="py-4 text-center text-nofx-text-muted"
                >
                  {t('decisionOutcomes.empty', language)}
                </td>
              </tr>
            )}
            {display.map((o) => {
              const pnl = o.realized_pnl
              const cum = o.cumulative_pnl
              const hasPos = (o.position_id ?? 0) > 0
              return (
                <tr
                  key={o.decision_id}
                  className="border-b border-white/5 hover:bg-white/5"
                >
                  <td className="py-1.5 text-nofx-text-muted">
                    #{o.cycle_number}
                  </td>
                  <td className="py-1.5 text-nofx-text-muted whitespace-nowrap">
                    {new Date(o.timestamp).toLocaleString()}
                  </td>
                  <td className="py-1.5">
                    {o.strategy_version_num > 0 ? (
                      <span
                        className="text-[10px] px-1 py-0.5 rounded"
                        style={{
                          background:
                            o.strategy_change_source === 'optimizer'
                              ? 'rgba(168, 85, 247, 0.15)'
                              : 'rgba(34, 197, 94, 0.15)',
                          color:
                            o.strategy_change_source === 'optimizer'
                              ? '#a855f7'
                              : '#22c55e',
                        }}
                      >
                        v{o.strategy_version_num}
                      </span>
                    ) : (
                      <span className="text-nofx-text-muted">-</span>
                    )}
                  </td>
                  <td className="py-1.5">
                    {hasPos ? (
                      <span>
                        {o.symbol}{' '}
                        <span
                          className={
                            o.side === 'LONG'
                              ? 'text-nofx-green'
                              : 'text-red-400'
                          }
                        >
                          {o.side}
                        </span>
                      </span>
                    ) : (
                      <span className="text-nofx-text-muted">—</span>
                    )}
                  </td>
                  <td className="py-1.5 text-right text-nofx-text-muted">
                    {hasPos && o.entry_price ? o.entry_price.toFixed(4) : '—'}
                  </td>
                  <td className="py-1.5 text-right text-nofx-text-muted">
                    {hasPos && o.exit_price ? o.exit_price.toFixed(4) : '—'}
                  </td>
                  <td
                    className={`py-1.5 text-right ${
                      o.is_closed
                        ? pnl >= 0
                          ? 'text-nofx-green'
                          : 'text-red-400'
                        : 'text-nofx-text-muted'
                    }`}
                  >
                    {o.is_closed
                      ? `${pnl >= 0 ? '+' : ''}${pnl.toFixed(2)}`
                      : '—'}
                  </td>
                  <td
                    className={`py-1.5 text-right ${
                      cum >= 0 ? 'text-nofx-green' : 'text-red-400'
                    }`}
                  >
                    {cum >= 0 ? '+' : ''}
                    {cum.toFixed(2)}
                  </td>
                  <td className="py-1.5 text-nofx-text-muted">
                    {o.position_status || (hasPos ? 'OPEN' : '—')}
                    {o.close_reason ? ` (${o.close_reason})` : ''}
                  </td>
                </tr>
              )
            })}
          </tbody>
        </table>
      </div>

      {/* Limit selector */}
      <div className="flex items-center justify-end gap-2 text-xs text-nofx-text-muted">
        <span>{t('decisionOutcomes.show', language)}:</span>
        {[20, 50, 100, 200].map((n) => (
          <button
            key={n}
            type="button"
            onClick={() => setLimit(n)}
            className={`px-2 py-0.5 rounded transition-colors ${
              limit === n
                ? 'bg-nofx-gold/20 text-nofx-gold'
                : 'hover:bg-white/10'
            }`}
          >
            {n}
          </button>
        ))}
      </div>
    </div>
  )
}
