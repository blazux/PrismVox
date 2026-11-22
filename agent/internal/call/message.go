package call

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"

	"phoneagent/internal/db"
)

// recordMessage confirms only a durable note in this call's log, never delivery.
// A failed replacement must leave the earlier captured message intact.
func (h *Handler) recordMessage(ctx context.Context, args map[string]interface{}) (string, error) {
	failed := func(err error) (string, error) {
		h.toolsUsed = append(h.toolsUsed, map[string]interface{}{"tool": "take_message", "args": args, "outcome": "failed"})
		return "Je suis désolée, je n'ai pas pu enregistrer le message.", err
	}
	recipient := strings.TrimSpace(stringFromMap(args, "recipient"))
	content := strings.TrimSpace(stringFromMap(args, "content"))
	if recipient == "" || content == "" {
		return failed(errors.New("message recipient and content are required"))
	}
	message := fmt.Sprintf("Pour %s : %s", recipient, content)
	if err := db.SaveCallMessage(ctx, h.pool, h.logID, message); err != nil {
		// Database errors can include rejected row values. Keep the phone/tool error
		// safe for routine logs, without claiming that the write succeeded.
		return failed(errors.New("message could not be stored"))
	}
	h.message = &message
	if h.llmClient != nil && h.llmClient.Docked() {
		h.llmClient.GatewayContext = append(h.llmClient.GatewayContext, "Message pour "+recipient+" noté dans le journal d'appel Vox ; personne n'est prévenu automatiquement.")
	}
	h.toolsUsed = append(h.toolsUsed, map[string]interface{}{"tool": "take_message", "args": args, "outcome": "recorded_in_call_log_not_delivered"})
	log.Printf("[%s] Message saved", h.channelID)
	return fmt.Sprintf("Le message pour %s est bien noté.", recipient), nil
}
