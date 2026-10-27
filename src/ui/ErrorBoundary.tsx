import { Component, type ErrorInfo, type ReactNode } from "react";
import { Button } from "./Button";

type Props = { children: ReactNode };
type State = { error: Error | null };

/** Global error boundary — replaces the old app's silent failure mode. */
export class ErrorBoundary extends Component<Props, State> {
  state: State = { error: null };

  static getDerivedStateFromError(error: Error): State {
    return { error };
  }
  componentDidCatch(error: Error, info: ErrorInfo) {
    console.error("Uncaught UI error:", error, info);
  }

  render() {
    if (this.state.error) {
      return (
        <div className="h-full flex items-center justify-center p-8">
          <div className="panel rounded-card p-6 max-w-lg">
            <h1 className="text-lg font-semibold text-danger mb-2">
              Something went wrong
            </h1>
            <pre className="text-xs whitespace-pre-wrap opacity-70 mb-4">
              {this.state.error.message}
            </pre>
            <Button variant="primary" onClick={() => this.setState({ error: null })}>
              Dismiss
            </Button>
          </div>
        </div>
      );
    }
    return this.props.children;
  }
}
