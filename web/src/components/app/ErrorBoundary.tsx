import { Component, type ErrorInfo, type ReactNode } from "react";
import { reportError } from "@/lib/telemetry";
import { ErrorState } from "./common";

export class ErrorBoundary extends Component<{ children: ReactNode; reset?: unknown }, { error: unknown }> {
  state = { error: null as unknown };
  static getDerivedStateFromError(error: unknown) {
    return { error };
  }
  componentDidCatch(error: unknown, info: ErrorInfo) {
    // React hands render errors to the boundary, so the browser's global handlers never see them.
    reportError(error, { componentStack: info.componentStack });
  }
  componentDidUpdate(prev: { reset?: unknown }) {
    if (prev.reset !== this.props.reset && this.state.error) this.setState({ error: null });
  }
  render() {
    return this.state.error ? <ErrorState error={this.state.error} retry={() => this.setState({ error: null })} /> : this.props.children;
  }
}
