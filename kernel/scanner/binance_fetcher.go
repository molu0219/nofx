package scanner

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// BinanceTickerEndpoint mirrors kernel/binance_top.go's choice. Duplicated
// here so the scanner package doesn't have to import the parent package
// (the parent will eventually depend on this one — would cycle).
const BinanceTickerEndpoint = "https://fapi.binance.com/fapi/v1/ticker/24hr"

// BinancePremiumIndexEndpoint returns mark / index / funding rate for every
// USDT-M perp in one call. Used by the funding-rate fetcher.
const BinancePremiumIndexEndpoint = "https://fapi.binance.com/fapi/v1/premiumIndex"

const fetchTimeout = 12 * time.Second

// httpClient is shared between the two fetchers; default Go transport reuses
// connections so back-to-back calls (ticker + funding) cost ~one TCP RTT.
var httpClient = &http.Client{Timeout: fetchTimeout}

// BinanceTickerFetcher returns a FetchTickersFunc bound to Binance's public
// 24h ticker endpoint. USDT-margined pairs only; leveraged tokens (UP/DOWN)
// filtered out — same hygiene as kernel/binance_top.go.
func BinanceTickerFetcher() FetchTickersFunc {
	return func(ctx context.Context) ([]TickerSnapshot, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, BinanceTickerEndpoint, nil)
		if err != nil {
			return nil, fmt.Errorf("binance ticker request: %w", err)
		}
		req.Header.Set("Accept", "application/json")

		resp, err := httpClient.Do(req)
		if err != nil {
			return nil, fmt.Errorf("binance ticker http: %w", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("binance ticker status %d", resp.StatusCode)
		}

		var raw []struct {
			Symbol             string `json:"symbol"`
			LastPrice          string `json:"lastPrice"`
			PriceChangePercent string `json:"priceChangePercent"`
			QuoteVolume        string `json:"quoteVolume"`
			HighPrice          string `json:"highPrice"`
			LowPrice           string `json:"lowPrice"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
			return nil, fmt.Errorf("binance ticker decode: %w", err)
		}

		out := make([]TickerSnapshot, 0, len(raw))
		for _, r := range raw {
			if !strings.HasSuffix(r.Symbol, "USDT") {
				continue
			}
			if strings.HasSuffix(r.Symbol, "UPUSDT") || strings.HasSuffix(r.Symbol, "DOWNUSDT") {
				continue
			}
			price, _ := strconv.ParseFloat(r.LastPrice, 64)
			vol, _ := strconv.ParseFloat(r.QuoteVolume, 64)
			pct, _ := strconv.ParseFloat(r.PriceChangePercent, 64)
			high, _ := strconv.ParseFloat(r.HighPrice, 64)
			low, _ := strconv.ParseFloat(r.LowPrice, 64)
			if price <= 0 || vol <= 0 {
				continue
			}
			out = append(out, TickerSnapshot{
				Symbol:            r.Symbol,
				Price:             price,
				QuoteVolume24h:    vol,
				PriceChange24hPct: pct,
				HighPrice24h:      high,
				LowPrice24h:       low,
			})
		}
		return out, nil
	}
}

// BinanceFundingFetcher returns a FetchFundingFunc bound to Binance's
// premiumIndex endpoint, returning per-symbol funding rates (decimal, e.g.
// 0.0001 = 0.01% per 8h).
func BinanceFundingFetcher() FetchFundingFunc {
	return func(ctx context.Context) (map[string]float64, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, BinancePremiumIndexEndpoint, nil)
		if err != nil {
			return nil, fmt.Errorf("binance funding request: %w", err)
		}
		req.Header.Set("Accept", "application/json")
		resp, err := httpClient.Do(req)
		if err != nil {
			return nil, fmt.Errorf("binance funding http: %w", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("binance funding status %d", resp.StatusCode)
		}

		var raw []struct {
			Symbol      string `json:"symbol"`
			LastFunding string `json:"lastFundingRate"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
			return nil, fmt.Errorf("binance funding decode: %w", err)
		}

		out := make(map[string]float64, len(raw))
		for _, r := range raw {
			rate, err := strconv.ParseFloat(r.LastFunding, 64)
			if err != nil {
				continue
			}
			out[r.Symbol] = rate
		}
		return out, nil
	}
}
