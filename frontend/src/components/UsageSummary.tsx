import type { Usage } from '../generated'
import { cacheRate } from '../state/chatStore'
import { Badge } from './ui/badge'
import { Button } from './ui/button'
import { Popover, PopoverContent, PopoverTrigger } from './ui/popover'
import { Separator } from './ui/separator'

export interface UsageSummaryProps {
  // The current/most-recent turn's model chip(s) - see chatStore.sessionModels.
  models: string[]
  // Chat-wide token aggregate (ChatDetail.usage) - a load-time snapshot, not
  // updated live while a run streams.
  usage?: Usage
}

function UsageBreakdown({ usage, cost, rate }: { usage?: Usage; cost?: number; rate: number | null | undefined }) {
  return (
    <>
      <UsageRow label="Input" value={usage?.input_tokens} />
      <UsageRow label="Output" value={usage?.output_tokens} />
      <UsageRow label="Reasoning" value={usage?.reasoning_tokens} />
      <UsageRow label="Cached" value={usage?.cached_tokens} />
      {cost != null && (
        <div className="flex items-center justify-between" title="Cloud-equivalent price of this chat's model calls">
          <span>Cost</span>
          <span className="tabular-nums">{fmtCost(cost)}</span>
        </div>
      )}
      {rate != null && (
        <>
          <Separator className="my-1" />
          <div className="flex items-center justify-between">
            <span>Cache rate</span>
            <span className="tabular-nums">{rate}%</span>
          </div>
        </>
      )}
    </>
  )
}

// Expands in a popover to the token split and cache-hit rate; `inline` lists them as static rows instead, for a
// menu where a nested popover trigger would be unreachable by keyboard. Renders nothing for a chat with no run yet.
export function UsageSummary({ models, usage, inline }: UsageSummaryProps & { inline?: boolean }) {
  const total = usage?.total_tokens ?? 0
  if (models.length === 0 && total <= 0) return null
  const rate = cacheRate(usage)
  const cost = usage?.cost_usd
  const totalText = `${total.toLocaleString()} tokens${cost != null ? ` · ${fmtCost(cost)}` : ''}`

  if (inline) {
    return (
      <div className="space-y-1 text-2xs text-muted-foreground">
        <div className="flex flex-wrap gap-2">
          {models.map(m => <Badge key={m} className="font-mono truncate max-w-[140px]" title={m}>{m}</Badge>)}
        </div>
        {total > 0 && <div className="tabular-nums text-foreground">{totalText}</div>}
        {total > 0 && <UsageBreakdown usage={usage} cost={cost} rate={rate} />}
      </div>
    )
  }

  return (
    <div className="flex items-center gap-2 text-2xs text-muted-foreground flex-shrink-0">
      {models.map(m => (
        <Badge key={m} className="font-mono truncate max-w-[140px]" title={m}>{m}</Badge>
      ))}
      {total > 0 && (
        <Popover>
          <PopoverTrigger asChild>
            <Button variant="link" size="inline" className="tabular-nums text-2xs text-muted-foreground hover:text-foreground hover:no-underline" title="Session token usage - click to expand">
              {totalText}
            </Button>
          </PopoverTrigger>
          <PopoverContent align="end" className="w-52 space-y-1 p-3 text-foreground">
            <UsageBreakdown usage={usage} cost={cost} rate={rate} />
          </PopoverContent>
        </Popover>
      )}
    </div>
  )
}

function UsageRow({ label, value }: { label: string; value?: number }) {
  if (value == null) return null
  return (
    <div className="flex items-center justify-between">
      <span>{label}</span>
      <span className="tabular-nums">{value.toLocaleString()}</span>
    </div>
  )
}

// Sub-cent chats read "<$0.01", not "$0.00", so a priced chat never looks free.
export function fmtCost(usd: number): string {
  return usd > 0 && usd < 0.01 ? '<$0.01' : `$${usd.toFixed(2)}`
}
