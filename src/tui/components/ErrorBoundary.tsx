import React from "react";
import { Box, Text } from "ink";

interface ErrorBoundaryProps {
  children: React.ReactNode;
}

interface ErrorBoundaryState {
  hasError: boolean;
  error?: Error;
}

/**
 * Error boundary component for graceful TUI error handling.
 * Catches React errors and displays a user-friendly error screen
 * instead of crashing the entire application.
 */
export class ErrorBoundary extends React.Component<
  ErrorBoundaryProps,
  ErrorBoundaryState
> {
  constructor(props: ErrorBoundaryProps) {
    super(props);
    this.state = { hasError: false };
  }

  static getDerivedStateFromError(error: Error): ErrorBoundaryState {
    return { hasError: true, error };
  }

  componentDidCatch(error: Error, errorInfo: React.ErrorInfo): void {
    // Log error details for debugging
    console.error("TUI Error Boundary caught an error:", error, errorInfo);
  }

  render(): React.ReactNode {
    if (this.state.hasError) {
      return (
        <Box flexDirection="column" padding={2} borderStyle="round" borderColor="red">
          <Text bold color="red">
            ⚠ TUI Error
          </Text>
          <Text> </Text>
          <Text color="yellow">An unexpected error occurred:</Text>
          <Text color="gray">{this.state.error?.message || "Unknown error"}</Text>
          <Text> </Text>
          <Text dimColor>Stack trace:</Text>
          <Text dimColor>{this.state.error?.stack || "No stack trace available"}</Text>
          <Text> </Text>
          <Text color="cyan">Press Q to quit and restart the application.</Text>
        </Box>
      );
    }

    return this.props.children;
  }
}
