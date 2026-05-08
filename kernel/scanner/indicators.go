package scanner

import "math"

// indicators.go: small, self-contained implementations of the indicators the
// AI scorer wants to see in the per-coin block. Math mirrors market/data_indicators.go
// (which uses CoinAnk-fetched klines for the deep trading-cycle analysis).
// Duplicated rather than imported so the scanner package stays free of
// circular deps with the parent kernel/market layer.

// closes extracts the close-price slice from a kline window.
func closes(klines []Kline) []float64 {
	out := make([]float64, len(klines))
	for i, k := range klines {
		out[i] = k.Close
	}
	return out
}

// calcEMA computes a simple exponential moving average. Returns 0 if the
// series is shorter than the period.
func calcEMA(values []float64, period int) float64 {
	if len(values) < period || period <= 0 {
		return 0
	}
	// Seed with SMA over the first `period` values.
	sum := 0.0
	for i := 0; i < period; i++ {
		sum += values[i]
	}
	ema := sum / float64(period)
	mul := 2.0 / float64(period+1)
	for i := period; i < len(values); i++ {
		ema = (values[i]-ema)*mul + ema
	}
	return ema
}

// calcMACDLine = EMA(12) - EMA(26). Standard MACD line, no signal/histogram —
// for prompt brevity we just give the line value.
func calcMACDLine(klines []Kline) float64 {
	c := closes(klines)
	if len(c) < 26 {
		return 0
	}
	return calcEMA(c, 12) - calcEMA(c, 26)
}

// calcRSI is Wilder's RSI over `period` candles. Returns 0 when there isn't
// enough history (need period+1 closes minimum).
func calcRSI(klines []Kline, period int) float64 {
	c := closes(klines)
	if len(c) <= period || period <= 0 {
		return 0
	}
	gains := 0.0
	losses := 0.0
	// Initial average gain/loss over the first `period` deltas.
	for i := 1; i <= period; i++ {
		d := c[i] - c[i-1]
		if d >= 0 {
			gains += d
		} else {
			losses -= d
		}
	}
	avgGain := gains / float64(period)
	avgLoss := losses / float64(period)
	// Smooth subsequent deltas Wilder-style.
	for i := period + 1; i < len(c); i++ {
		d := c[i] - c[i-1]
		gain := 0.0
		loss := 0.0
		if d >= 0 {
			gain = d
		} else {
			loss = -d
		}
		avgGain = (avgGain*float64(period-1) + gain) / float64(period)
		avgLoss = (avgLoss*float64(period-1) + loss) / float64(period)
	}
	if avgLoss == 0 {
		if avgGain == 0 {
			return 50
		}
		return 100
	}
	rs := avgGain / avgLoss
	return 100 - (100 / (1 + rs))
}

// calcATR is true range smoothed over `period` candles. Returns 0 when
// history is insufficient.
func calcATR(klines []Kline, period int) float64 {
	if len(klines) <= period || period <= 0 {
		return 0
	}
	trs := make([]float64, 0, len(klines)-1)
	for i := 1; i < len(klines); i++ {
		hl := klines[i].High - klines[i].Low
		hc := math.Abs(klines[i].High - klines[i-1].Close)
		lc := math.Abs(klines[i].Low - klines[i-1].Close)
		trs = append(trs, math.Max(hl, math.Max(hc, lc)))
	}
	if len(trs) < period {
		return 0
	}
	// Wilder smoothing seeded with arithmetic mean of the first `period` TRs.
	sum := 0.0
	for i := 0; i < period; i++ {
		sum += trs[i]
	}
	atr := sum / float64(period)
	for i := period; i < len(trs); i++ {
		atr = (atr*float64(period-1) + trs[i]) / float64(period)
	}
	return atr
}
