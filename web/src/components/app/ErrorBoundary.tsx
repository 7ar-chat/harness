import { Component, type ReactNode } from "react";
import { ErrorState } from "./common";

export class ErrorBoundary extends Component<{ children: ReactNode; reset?: unknown }, { error: unknown }> {
  state = { error: null as unknown };
  static getDerivedStateFromError(error: unknown) {
    return { error };
  }
  componentDidUpdate(prev: { reset?: unknown }) {
    if (prev.reset !== this.props.reset && this.state.error) this.setState({ error: null });
  }
  render() {
    return this.state.error ? <ErrorState error={this.state.error} retry={() => this.setState({ error: null })} /> : this.props.children;
  }
}
