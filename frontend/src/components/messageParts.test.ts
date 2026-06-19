import { describe, it, expect } from 'vitest'
import { showLiveSpinner, startRun, type AgentRun } from './messageParts'

describe('showLiveSpinner', () => {
  it('shows dots while streaming before anything visible arrives', () => {
    expect(showLiveSpinner({ streaming: true, hasDag: false, answerText: '', visibleActivityCount: 0 })).toBe(true)
  })

  it('keeps dots when an empty orchestrator run exists but has no visible activity yet', () => {
    // Regression: a top-level run is created (empty) on the first stream event to
    // hold the orchestrator's plan/execute tool calls. The spinner is keyed on
    // visible activity, not run count, so it stays up during the pre-plan gap.
    const runs: AgentRun[] = startRun([], { runId: 'orchestrator', agent: 'orchestrator', stage: 'worker' })
    expect(runs).toHaveLength(1)
    expect(runs[0].activity).toHaveLength(0)
    expect(showLiveSpinner({ streaming: true, hasDag: false, answerText: '', visibleActivityCount: runs[0].activity.length })).toBe(true)
  })

  it('hides dots once visible activity arrives', () => {
    expect(showLiveSpinner({ streaming: true, hasDag: false, answerText: '', visibleActivityCount: 1 })).toBe(false)
  })

  it('hides dots once the DAG or answer text arrives', () => {
    expect(showLiveSpinner({ streaming: true, hasDag: true, answerText: '', visibleActivityCount: 0 })).toBe(false)
    expect(showLiveSpinner({ streaming: true, hasDag: false, answerText: 'hi', visibleActivityCount: 0 })).toBe(false)
  })

  it('never shows dots when not streaming', () => {
    expect(showLiveSpinner({ streaming: false, hasDag: false, answerText: '', visibleActivityCount: 0 })).toBe(false)
  })
})
