package resilience

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"time"
)

// Alerter sends webhook notifications on important state changes.
// Supports Slack-compatible webhook payloads.
//
// To enable, set the webhook URL. To disable, leave it empty.
// Alerting is opt-in — creating an Alerter with an empty URL is a no-op.
type Alerter struct {
	webhookURL string
	client     *http.Client
}

// NewAlerter creates a new alerter.
// Returns a no-op alerter if webhookURL is empty.
func NewAlerter(webhookURL string) *Alerter {
	if webhookURL == "" {
		log.Printf("[alerter] No webhook URL — alerting disabled")
	} else {
		log.Printf("[alerter] Webhook alerting enabled")
	}
	return &Alerter{
		webhookURL: webhookURL,
		client:     &http.Client{Timeout: 10 * time.Second},
	}
}

// slackPayload is a Slack Incoming Webhook message format.
type slackPayload struct {
	Text        string       `json:"text"`
	Attachments []attachment `json:"attachments,omitempty"`
}

type attachment struct {
	Color  string `json:"color"`
	Title  string `json:"title"`
	Text   string `json:"text"`
	Footer string `json:"footer"`
	Ts     int64  `json:"ts"`
}

// WireToCircuitBreaker connects this alerter to a circuit breaker's
// OnStateChange callback. After calling this, the alerter automatically
// fires webhooks on CB state transitions.
func (a *Alerter) WireToCircuitBreaker(cb *CircuitBreaker) {
	if a.webhookURL == "" {
		return // No-op
	}
	cb.OnStateChange = a.SendCircuitBreakerAlert
}

// SendCircuitBreakerAlert fires a Slack webhook when the circuit breaker
// transitions to OPEN (failures) or back to CLOSED (recovered).
func (a *Alerter) SendCircuitBreakerAlert(fromState, toState string, consecutiveFailures int, lastErr string) {
	if a == nil || a.webhookURL == "" {
		return
	}

	var color, emoji, title string
	switch toState {
	case "OPEN":
		color = "danger"
		emoji = "🚨"
		title = "Circuit Breaker OPEN — Operations Halted"
	case "CLOSED":
		color = "good"
		emoji = "✅"
		title = "Circuit Breaker CLOSED — Operations Resumed"
	case "HALF-OPEN":
		color = "warning"
		emoji = "⚠️"
		title = "Circuit Breaker HALF-OPEN — Probing"
	default:
		color = "#808080"
		emoji = "ℹ️"
		title = fmt.Sprintf("Circuit Breaker → %s", toState)
	}

	text := fmt.Sprintf("%s *soqucoin-sdk*: %s → %s", emoji, fromState, toState)
	if lastErr != "" {
		text += fmt.Sprintf("\n> Last error: `%s`", lastErr)
	}
	if consecutiveFailures > 0 {
		text += fmt.Sprintf("\n> Consecutive failures: %d", consecutiveFailures)
	}

	payload := slackPayload{
		Text: text,
		Attachments: []attachment{
			{
				Color:  color,
				Title:  title,
				Text:   fmt.Sprintf("Transition: %s → %s\nFailures: %d", fromState, toState, consecutiveFailures),
				Footer: "soqucoin-sdk",
				Ts:     time.Now().Unix(),
			},
		},
	}

	go a.send(payload)
}

// SendAlert sends a generic alert message via webhook.
func (a *Alerter) SendAlert(title, message, color string) {
	if a == nil || a.webhookURL == "" {
		return
	}

	payload := slackPayload{
		Text: fmt.Sprintf("ℹ️ *soqucoin-sdk*: %s", title),
		Attachments: []attachment{
			{
				Color:  color,
				Title:  title,
				Text:   message,
				Footer: "soqucoin-sdk",
				Ts:     time.Now().Unix(),
			},
		},
	}

	go a.send(payload)
}

// send posts the payload to the webhook URL (fire-and-forget).
func (a *Alerter) send(payload slackPayload) {
	body, err := json.Marshal(payload)
	if err != nil {
		log.Printf("[alerter] ERROR: marshal payload: %v", err)
		return
	}

	resp, err := a.client.Post(a.webhookURL, "application/json", bytes.NewReader(body))
	if err != nil {
		log.Printf("[alerter] ERROR: webhook POST failed: %v", err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		log.Printf("[alerter] WARNING: webhook returned HTTP %d", resp.StatusCode)
		return
	}

	log.Printf("[alerter] Webhook sent successfully")
}
