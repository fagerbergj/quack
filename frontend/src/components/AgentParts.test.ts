import { describe, it, expect } from 'vitest'
import {
  appendTextPart,
  appendThinkingPart,
  appendToolCall,
  fillToolResult,
  partsToText,
  type MessagePart,
} from './AgentParts'

describe('AgentParts helpers', () => {
  it('coalesces consecutive streamed text', () => {
    let parts: MessagePart[] = []
    parts = appendTextPart(parts, 'Hello ')
    parts = appendTextPart(parts, 'world')
    expect(parts).toEqual([{ kind: 'text', text: 'Hello world' }])
  })

  it('opens a new part when the kind changes', () => {
    let parts: MessagePart[] = []
    parts = appendThinkingPart(parts, 'reasoning')
    parts = appendTextPart(parts, 'answer')
    expect(parts.map(p => p.kind)).toEqual(['thinking', 'text'])
  })

  it('fills a tool result onto the matching pending call', () => {
    let parts: MessagePart[] = []
    parts = appendToolCall(parts, 'current_time', {})
    parts = fillToolResult(parts, 'current_time', { result: 'now' })
    const tool = parts[0]
    expect(tool.kind).toBe('tool_call')
    if (tool.kind === 'tool_call') expect(tool.result).toEqual({ result: 'now' })
  })

  it('partsToText includes only text parts', () => {
    let parts: MessagePart[] = []
    parts = appendThinkingPart(parts, 'reasoning')
    parts = appendTextPart(parts, 'the answer')
    parts = appendToolCall(parts, 't', {})
    expect(partsToText(parts)).toBe('the answer')
  })
})
