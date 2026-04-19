package commands

import "context"

// newCommand starts a fresh conversation by clearing chat history and
// summary for the current session. Equivalent to /clear; added because
// "/new" is the more intuitive verb for users.
func newCommand() Definition {
	return Definition{
		Name:        "new",
		Description: "Start a new conversation",
		Usage:       "/new",
		Handler: func(_ context.Context, req Request, rt *Runtime) error {
			if rt == nil || rt.ClearHistory == nil {
				return req.Reply(unavailableMsg)
			}
			if err := rt.ClearHistory(); err != nil {
				return req.Reply("Failed to start new conversation: " + err.Error())
			}
			return req.Reply("Conversation cleared ✨")
		},
	}
}
