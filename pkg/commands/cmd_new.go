package commands

import "context"

func newCommand() Definition {
	return Definition{
		Name:        "new",
		Description: "Start a new conversation",
		Usage:       "/new",
		Aliases:     []string{"clear"},
		Handler: func(ctx context.Context, req Request, rt *Runtime) error {
			if rt == nil || rt.ResetSession == nil {
				return req.Reply(unavailableMsg)
			}
			if err := rt.ResetSession(ctx); err != nil {
				return req.Reply("Failed to reset session: " + err.Error())
			}
			return req.Reply("Conversation cleared ✨")
		},
	}
}
