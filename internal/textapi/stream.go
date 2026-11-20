package textapi

import (
	"context"
	"errors"
	"strings"

	"github.com/basketikun/chatgpt2api/internal/imagegen"
	"github.com/basketikun/chatgpt2api/internal/pool"
	"github.com/basketikun/chatgpt2api/internal/settings"
)

// SearchModel is openai_search.MODEL.
const SearchModel = "gpt-5-5"

// Backend is one account's (or the anonymous) ChatGPT web client.
type Backend interface {
	StreamText(ctx context.Context, messages []map[string]any, model, thinkingEffort string) (<-chan string, func() error, error)
	ListModels(ctx context.Context) (map[string]any, error)
	Search(ctx context.Context, prompt string) (map[string]any, error)
}

// Deps are what the text endpoints need.
type Deps struct {
	Settings   *settings.Store
	Pool       *pool.Service
	NewBackend func(token string) (Backend, error)
	Generator  *imagegen.Generator // image requests that arrive through the text endpoints
	Images     Images
	Cache      *Cache
	Catalog    *Catalog
}

// textToken is account_service.get_text_access_token with the model's catalog route.
func (d *Deps) textToken(ctx context.Context, model string, excluded map[string]bool) (string, error) {
	var route *pool.ModelRoute
	if model = strings.TrimSpace(model); model != "" && model != "auto" {
		route = d.Catalog.RouteForModel(ctx, model)
	}
	return d.Pool.GetTextAccessToken(ctx, excluded, model, route)
}

// textRequest is the ConversationRequest of a text call.
type textRequest struct {
	Model          string
	Messages       []map[string]any
	ThinkingEffort string
}

func assistantHistory(messages []map[string]any) (string, []string) {
	text, list := "", []string{}
	for _, message := range messages {
		if message["role"] != "assistant" {
			continue
		}
		content := pyStr(message["content"])
		if _, isList := message["content"].([]any); isList {
			content = "" // a list never reaches here for assistants; Python would str() it
		}
		text += content
		if content != "" {
			list = append(list, content)
		}
	}
	return text, list
}

// streamTextDeltas is stream_text_deltas: run the conversation with the given token; when the
// token turns out invalid before any text arrived, refresh it or move to another account.
func (d *Deps) streamTextDeltas(ctx context.Context, token string, request textRequest, emit func(string) error) error {
	attempted := map[string]bool{}
	emitted := false
	for {
		if token != "" && attempted[token] {
			return errors.New("no available text account")
		}
		if token != "" {
			attempted[token] = true
		}
		err := d.runConversation(ctx, token, request, func(delta string) error {
			emitted = true
			return emit(delta)
		})
		if err == nil {
			d.Pool.MarkTextUsed(token)
			return nil
		}
		message := err.Error()
		if token == "" || emitted || !imagegen.IsTokenInvalidError(message) {
			return err
		}
		refreshed := d.Pool.RefreshAccessToken(ctx, token, true, "text_stream")
		if refreshed != "" && refreshed != token && !attempted[refreshed] {
			token = refreshed
		} else {
			d.Pool.RemoveInvalidToken(token, "text_stream", false, message)
			next, nextErr := d.textToken(ctx, request.Model, attempted)
			if nextErr != nil {
				return nextErr
			}
			token = next
		}
		if token == "" {
			return err
		}
	}
}

// runConversation is one conversation_events pass, emitting the text deltas.
func (d *Deps) runConversation(ctx context.Context, token string, request textRequest, emit func(string) error) error {
	backend, err := d.NewBackend(token)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	normalized := request.Messages
	history, historyMessages := assistantHistory(normalized)
	payloads, wait, err := backend.StreamText(ctx, normalized, request.Model, request.ThinkingEffort)
	if err != nil {
		return err
	}
	var emitErr error
	imagegen.IterConversationPayloads(payloads, history, historyMessages, func(event imagegen.Event) {
		if emitErr != nil || event.Type != "conversation.delta" || event.Delta == "" {
			return
		}
		if emitErr = emit(event.Delta); emitErr != nil {
			cancel()
		}
	})
	streamErr := wait()
	if emitErr != nil {
		return emitErr
	}
	return streamErr
}

// collectText is collect_text.
func (d *Deps) collectText(ctx context.Context, token string, request textRequest) (string, error) {
	var builder strings.Builder
	err := d.streamTextDeltas(ctx, token, request, func(delta string) error {
		builder.WriteString(delta)
		return nil
	})
	return builder.String(), err
}
