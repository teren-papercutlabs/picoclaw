// Package channels - agent_executor.go
//
// AgentExecutor is the interface channels implement to invoke the agent loop
// directly. This is used by inbound-only channels (e.g. HTTP webhook) that
// need to process a message through the agent without going through the
// normal channel bus dispatch.
//
// AgentLoop satisfies this via ProcessDirectWithChannel.
package channels

import "context"

// AgentExecutor is the interface for channels that need to invoke the agent loop directly.
type AgentExecutor interface {
	ProcessDirectWithChannel(ctx context.Context, content, sessionKey, channel, chatID string) (string, error)
}

// AgentExecutorSetter is implemented by channels that accept an injected
// AgentExecutor. The Manager calls SetExecutor on channels implementing this
// interface after they are constructed.
type AgentExecutorSetter interface {
	SetExecutor(exec AgentExecutor)
}
