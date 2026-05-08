import type {
  TraderInfo,
  TraderConfigData,
  CreateTraderRequest,
  DecisionOutcome,
  StrategyVersion,
} from '../../types'
import { API_BASE, httpClient } from './helpers'
import { ApiError } from '../httpClient'

function throwApiError(
  message: string,
  errorKey?: string,
  errorParams?: Record<string, string>,
  statusCode?: number
): never {
  throw new ApiError(message, errorKey, errorParams, statusCode)
}

export const traderApi = {
  async getTraders(silent?: boolean): Promise<TraderInfo[]> {
    const result = await httpClient.request<TraderInfo[]>(
      `${API_BASE}/my-traders`,
      { silent }
    )
    if (!result.success) throw new Error('Failed to fetch trader list')
    return Array.isArray(result.data) ? result.data : []
  },

  async getPublicTraders(): Promise<any[]> {
    const result = await httpClient.get<any[]>(`${API_BASE}/traders`)
    if (!result.success) throw new Error('Failed to fetch public trader list')
    return result.data!
  },

  async createTrader(request: CreateTraderRequest): Promise<TraderInfo> {
    const result = await httpClient.post<TraderInfo>(
      `${API_BASE}/traders`,
      request
    )
    if (!result.success) {
      throwApiError(
        result.message || 'Failed to create trader',
        result.errorKey,
        result.errorParams,
        result.statusCode
      )
    }
    return result.data!
  },

  async deleteTrader(traderId: string): Promise<void> {
    const result = await httpClient.delete(`${API_BASE}/traders/${traderId}`)
    if (!result.success) throw new Error('Failed to delete trader')
  },

  async startTrader(traderId: string): Promise<void> {
    const result = await httpClient.post(
      `${API_BASE}/traders/${traderId}/start`
    )
    if (!result.success) {
      throwApiError(
        result.message || 'Failed to start trader',
        result.errorKey,
        result.errorParams,
        result.statusCode
      )
    }
  },

  async stopTrader(traderId: string): Promise<void> {
    const result = await httpClient.post(`${API_BASE}/traders/${traderId}/stop`)
    if (!result.success) throw new Error('Failed to stop trader')
  },

  async toggleCompetition(
    traderId: string,
    showInCompetition: boolean
  ): Promise<void> {
    const result = await httpClient.put(
      `${API_BASE}/traders/${traderId}/competition`,
      { show_in_competition: showInCompetition }
    )
    if (!result.success)
      throw new Error('Failed to update competition visibility')
  },

  async closePosition(
    traderId: string,
    symbol: string,
    side: string
  ): Promise<{ message: string }> {
    const result = await httpClient.post<{ message: string }>(
      `${API_BASE}/traders/${traderId}/close-position`,
      { symbol, side }
    )
    if (!result.success) throw new Error('Failed to close position')
    return result.data!
  },

  // getDecisionOutcomes returns recent decisions joined with their strategy
  // version and any position outcome. Powers the diagnostic panel — each
  // row tells you "this cycle made this decision under config v7, opened
  // this position, closed at this PnL".
  async getDecisionOutcomes(
    traderId: string,
    limit = 100
  ): Promise<DecisionOutcome[]> {
    const result = await httpClient.request<{ outcomes: DecisionOutcome[] }>(
      `${API_BASE}/traders/${traderId}/decision-outcomes?limit=${limit}`,
      { silent: true }
    )
    if (!result.success) throw new Error('Failed to fetch decision outcomes')
    return result.data?.outcomes ?? []
  },

  // getStrategyVersions returns the trader's strategy revision timeline,
  // oldest first. Each row = one config change with source + reasoning.
  async getStrategyVersions(
    traderId: string,
    limit = 50
  ): Promise<StrategyVersion[]> {
    const result = await httpClient.request<{ versions: StrategyVersion[] }>(
      `${API_BASE}/traders/${traderId}/strategy-versions?limit=${limit}`,
      { silent: true }
    )
    if (!result.success) throw new Error('Failed to fetch strategy versions')
    return result.data?.versions ?? []
  },

  // resetPaper wipes a paper trader's simulated state back to a clean
  // initial balance. Server-side only succeeds for traders bound to a paper
  // exchange (400 otherwise) — caller is expected to gate the UI control on
  // exchange.exchange_type === 'paper'.
  async resetPaper(traderId: string, initialBalance: number): Promise<void> {
    const result = await httpClient.post(
      `${API_BASE}/traders/${traderId}/paper/reset`,
      { initial_balance: initialBalance }
    )
    if (!result.success) {
      throwApiError(
        result.message || 'Failed to reset paper trader',
        result.errorKey,
        result.errorParams,
        result.statusCode
      )
    }
  },

  async updateTraderPrompt(
    traderId: string,
    customPrompt: string
  ): Promise<void> {
    const result = await httpClient.put(
      `${API_BASE}/traders/${traderId}/prompt`,
      { custom_prompt: customPrompt }
    )
    if (!result.success) throw new Error('Failed to update custom prompt')
  },

  async getTraderConfig(traderId: string): Promise<TraderConfigData> {
    const result = await httpClient.get<TraderConfigData>(
      `${API_BASE}/traders/${traderId}/config`
    )
    if (!result.success) throw new Error('Failed to fetch trader config')
    return result.data!
  },

  async updateTrader(
    traderId: string,
    request: CreateTraderRequest
  ): Promise<TraderInfo> {
    const result = await httpClient.put<TraderInfo>(
      `${API_BASE}/traders/${traderId}`,
      request
    )
    if (!result.success) {
      throwApiError(
        result.message || 'Failed to update trader',
        result.errorKey,
        result.errorParams,
        result.statusCode
      )
    }
    return result.data!
  },
}
