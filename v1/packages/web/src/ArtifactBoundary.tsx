import { Component, type ReactNode } from 'react';
import type { SealedArtifact } from './api.ts';

/** Draws an extension's rendering of an artifact; if it throws, a plain fallback shows instead of taking the session view down. */
export class ArtifactBoundary extends Component<{ artifact: SealedArtifact; children: ReactNode }, { failed: boolean }> {
  state = { failed: false };
  static getDerivedStateFromError() {
    return { failed: true };
  }
  componentDidUpdate(prev: { artifact: SealedArtifact }) {
    if (this.state.failed && prev.artifact !== this.props.artifact) this.setState({ failed: false });
  }
  render() {
    if (!this.state.failed) return this.props.children;
    return (
      <div role="alert" className="flex flex-col gap-2 text-[13px]">
        <p className="m-0 text-danger">This couldn't be drawn. Here is what was saved.</p>
        <pre className="m-0 max-h-80 overflow-auto rounded-xl border border-line bg-canvas p-3 font-mono text-xs whitespace-pre-wrap [overflow-wrap:anywhere]">{this.props.artifact.content}</pre>
      </div>
    );
  }
}
