import React from 'react'

interface IconProps {
  width?: number
  height?: number
  className?: string
}

// 本地图标路径映射
const ICON_PATHS: Record<string, string> = {
  binance: '/exchange-icons/binance.jpg',
  bybit: '/exchange-icons/bybit.png',
  okx: '/exchange-icons/okx.svg',
  bitget: '/exchange-icons/bitget.svg',
  gate: '/exchange-icons/gate.svg',
  kucoin: '/exchange-icons/kucoin.svg',
  hyperliquid: '/exchange-icons/hyperliquid.png',
  aster: '/exchange-icons/aster.svg',
  lighter: '/exchange-icons/lighter.png',
  indodax: '/exchange-icons/indodax.png',
}

// PaperIcon renders a distinct virtual-exchange marker using a paper emoji
// inside the same rounded surface used for real exchanges. Inline so we don't
// need to ship an extra asset for what is intentionally a non-real venue.
const PaperIcon: React.FC<IconProps> = ({
  width = 24,
  height = 24,
  className,
}) => (
  <div
    className={className}
    style={{
      width,
      height,
      borderRadius: 6,
      flexShrink: 0,
      background: 'linear-gradient(135deg, #1A1F2E 0%, #2B3139 100%)',
      border: '1px dashed rgba(240, 185, 11, 0.5)',
      display: 'flex',
      alignItems: 'center',
      justifyContent: 'center',
      fontSize: Math.max(12, (width || 24) * 0.55),
    }}
  >
    📄
  </div>
)

// 通用图标组件
const ExchangeImage: React.FC<IconProps & { src: string; alt: string }> = ({
  width = 24,
  height = 24,
  className,
  src,
  alt,
}) => (
  <div
    className={className}
    style={{
      width,
      height,
      borderRadius: 6,
      overflow: 'hidden',
      flexShrink: 0,
      background: '#2B3139',
    }}
  >
    <img
      src={src}
      alt={alt}
      style={{
        width: '100%',
        height: '100%',
        objectFit: 'cover',
      }}
    />
  </div>
)

// Fallback 图标
const FallbackIcon: React.FC<IconProps & { label: string }> = ({
  width = 24,
  height = 24,
  className,
  label,
}) => (
  <div
    className={className}
    style={{
      width,
      height,
      borderRadius: 6,
      background: '#2B3139',
      display: 'flex',
      alignItems: 'center',
      justifyContent: 'center',
      fontSize: Math.max(10, (width || 24) * 0.4),
      fontWeight: 'bold',
      color: '#EAECEF',
      flexShrink: 0,
    }}
  >
    {label[0]?.toUpperCase() || '?'}
  </div>
)

// 获取交易所图标的函数
export const getExchangeIcon = (
  exchangeType: string,
  props: IconProps = {}
) => {
  const lowerType = exchangeType.toLowerCase()
  const iconProps = {
    width: props.width || 24,
    height: props.height || 24,
    className: props.className,
  }

  // Paper trading is virtual — gets its own emoji-based marker instead of a logo.
  if (lowerType === 'paper' || lowerType.includes('paper')) {
    return <PaperIcon {...iconProps} />
  }

  const type = lowerType.includes('binance')
    ? 'binance'
    : lowerType.includes('bybit')
      ? 'bybit'
      : lowerType.includes('okx')
        ? 'okx'
        : lowerType.includes('bitget')
          ? 'bitget'
          : lowerType.includes('gate')
            ? 'gate'
            : lowerType.includes('kucoin')
              ? 'kucoin'
              : lowerType.includes('hyperliquid')
                ? 'hyperliquid'
                : lowerType.includes('aster')
                  ? 'aster'
                  : lowerType.includes('lighter')
                    ? 'lighter'
                    : lowerType.includes('indodax')
                      ? 'indodax'
                      : lowerType

  const path = ICON_PATHS[type]
  if (path) {
    return <ExchangeImage {...iconProps} src={path} alt={type} />
  }

  return <FallbackIcon {...iconProps} label={type} />
}
