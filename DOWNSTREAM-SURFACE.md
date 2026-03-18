# Downstream Surface

Generated from `git diff upstream/main..main --name-only --diff-filter=M`.
Review at every upstream sync.

Last generated: 2026-03-18

| File | Reason | Can we extract to shim? | Notes |
|------|--------|-------------------------|-------|
| (no upstream files modified yet) | | | |

## reply-to-bot group trigger (commit 0f95da7)
- **File**: `pkg/channels/telegram/telegram.go` (~line 499)
- **Change**: When `mention_only` is true in group chats, also respond if the message is a reply to one of the bot's own messages. Upstream only checks `isBotMentioned()`.
- **Reason**: Client groups need mention-or-reply behavior. Upstream's mention_only ignores replies entirely.
