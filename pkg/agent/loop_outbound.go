// PicoClaw - Ultra-lightweight personal AI agent

package agent

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/teren-papercutlabs/pclaw/pkg/bus"
	"github.com/teren-papercutlabs/pclaw/pkg/logger"
	"github.com/teren-papercutlabs/pclaw/pkg/tools"
)

func (al *AgentLoop) maybePublishError(ctx context.Context, channel, chatID, sessionKey, replyToMessageID string, err error) bool {
	if errors.Is(err, context.Canceled) {
		return false
	}
	al.publishResponseIfNeededInternal(ctx, channel, chatID, sessionKey, replyToMessageID, fmt.Sprintf("Error processing message: %v", err))
	return true
}

func (al *AgentLoop) publishResponseOrError(
	ctx context.Context,
	channel, chatID, sessionKey, replyToMessageID string,
	response string,
	err error,
) {
	if err != nil {
		if !al.maybePublishError(ctx, channel, chatID, sessionKey, replyToMessageID, err) {
			return
		}
		response = ""
	}
	al.publishResponseIfNeededInternal(ctx, channel, chatID, sessionKey, replyToMessageID, response)
}

// PublishResponseIfNeeded is the legacy entry point retained for the
// JobExecutor interface (cron, async fanout) that does not have an inbound
// message ID to thread to. Calls into the new internal publisher with no
// reply target.
func (al *AgentLoop) PublishResponseIfNeeded(ctx context.Context, channel, chatID, sessionKey, response string) {
	al.publishResponseIfNeededInternal(ctx, channel, chatID, sessionKey, "", response)
}

// PublishResponseIfNeededWithReplyTo publishes an agent reply, threading it
// to the inbound message ID when present. Used by the per-message agent loop
// so replies thread to the user's source message in group chats — matches
// the natural Telegram conversational pattern.
func (al *AgentLoop) PublishResponseIfNeededWithReplyTo(
	ctx context.Context,
	channel, chatID, sessionKey, replyToMessageID, response string,
) {
	al.publishResponseIfNeededInternal(ctx, channel, chatID, sessionKey, replyToMessageID, response)
}

func (al *AgentLoop) publishResponseIfNeededInternal(
	ctx context.Context,
	channel, chatID, sessionKey, replyToMessageID, response string,
) {
	if response == "" {
		return
	}

	alreadySentToSameChat := false
	defaultAgent := al.GetRegistry().GetDefaultAgent()
	if defaultAgent != nil {
		if tool, ok := defaultAgent.Tools.Get("message"); ok {
			if mt, ok := tool.(*tools.MessageTool); ok {
				alreadySentToSameChat = mt.HasSentTo(sessionKey, channel, chatID)
			}
		}
	}

	if alreadySentToSameChat {
		logger.DebugCF(
			"agent",
			"Skipped outbound (message tool already sent to same chat)",
			map[string]any{"channel": channel, "chat_id": chatID},
		)
		return
	}

	al.bus.PublishOutbound(ctx, bus.OutboundMessage{
		Context:          bus.NewOutboundContext(channel, chatID, replyToMessageID),
		Content:          response,
		ReplyToMessageID: replyToMessageID,
	})
	logger.InfoCF("agent", "Published outbound response",
		map[string]any{
			"channel":             channel,
			"chat_id":             chatID,
			"content_len":         len(response),
			"reply_to_message_id": replyToMessageID,
		})
}

func (al *AgentLoop) targetReasoningChannelID(channelName string) (chatID string) {
	if al.channelManager == nil {
		return ""
	}
	if ch, ok := al.channelManager.GetChannel(channelName); ok {
		return ch.ReasoningChannelID()
	}
	return ""
}

func (al *AgentLoop) publishPicoReasoning(ctx context.Context, reasoningContent, chatID string) {
	if reasoningContent == "" || chatID == "" {
		return
	}

	if ctx.Err() != nil {
		return
	}

	pubCtx, pubCancel := context.WithTimeout(ctx, 5*time.Second)
	defer pubCancel()

	if err := al.bus.PublishOutbound(pubCtx, bus.OutboundMessage{
		Context: bus.InboundContext{
			Channel: "pico",
			ChatID:  chatID,
			Raw: map[string]string{
				metadataKeyMessageKind: messageKindThought,
			},
		},
		Content: reasoningContent,
	}); err != nil {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) ||
			errors.Is(err, bus.ErrBusClosed) {
			logger.DebugCF("agent", "Pico reasoning publish skipped (timeout/cancel)", map[string]any{
				"channel": "pico",
				"error":   err.Error(),
			})
		} else {
			logger.WarnCF("agent", "Failed to publish pico reasoning (best-effort)", map[string]any{
				"channel": "pico",
				"error":   err.Error(),
			})
		}
	}
}

func (al *AgentLoop) handleReasoning(
	ctx context.Context,
	reasoningContent, channelName, channelID string,
) {
	if reasoningContent == "" || channelName == "" || channelID == "" {
		return
	}

	// Check context cancellation before attempting to publish,
	// since PublishOutbound's select may race between send and ctx.Done().
	if ctx.Err() != nil {
		return
	}

	// Use a short timeout so the goroutine does not block indefinitely when
	// the outbound bus is full.  Reasoning output is best-effort; dropping it
	// is acceptable to avoid goroutine accumulation.
	pubCtx, pubCancel := context.WithTimeout(ctx, 5*time.Second)
	defer pubCancel()

	if err := al.bus.PublishOutbound(pubCtx, bus.OutboundMessage{
		Context: bus.NewOutboundContext(channelName, channelID, ""),
		Content: reasoningContent,
	}); err != nil {
		// Treat context.DeadlineExceeded / context.Canceled as expected
		// (bus full under load, or parent canceled).  Check the error
		// itself rather than ctx.Err(), because pubCtx may time out
		// (5 s) while the parent ctx is still active.
		// Also treat ErrBusClosed as expected — it occurs during normal
		// shutdown when the bus is closed before all goroutines finish.
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) ||
			errors.Is(err, bus.ErrBusClosed) {
			logger.DebugCF("agent", "Reasoning publish skipped (timeout/cancel)", map[string]any{
				"channel": channelName,
				"error":   err.Error(),
			})
		} else {
			logger.WarnCF("agent", "Failed to publish reasoning (best-effort)", map[string]any{
				"channel": channelName,
				"error":   err.Error(),
			})
		}
	}
}
