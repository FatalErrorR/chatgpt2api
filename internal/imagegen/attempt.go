package imagegen

import (
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"time"

	"github.com/basketikun/chatgpt2api/internal/settings"
)

// Backend is the part of OpenAIBackendAPI the image pipeline uses, for one account.
type Backend interface {
	// StreamPicture uploads the references, starts the picture_v2 conversation and streams the SSE
	// data payloads into the channel. The returned wait gives the stream's error after the channel closes.
	StreamPicture(ctx context.Context, prompt, model string, images []string, size string) (<-chan string, func() error, error)
	// QueryTasks is _query_backend_tasks.
	QueryTasks(ctx context.Context, conversationID string, timeout time.Duration) ([]map[string]any, error)
	// CheckTaskError is check_task_error: (is_error, message).
	CheckTaskError(task map[string]any) (bool, string)
	// FindConversationByPrompt looks for the newest conversation made from this prompt.
	FindConversationByPrompt(ctx context.Context, prompt string, since time.Time, timeout time.Duration) (string, error)
	// ResolveImageURLs is resolve_conversation_image_urls: poll (when asked) until ids appear,
	// then turn them into download URLs. Returns *ContentPolicyError or *PollTimeoutError.
	ResolveImageURLs(ctx context.Context, conversationID string, fileIDs, sedimentIDs []string, pollTimeoutSecs float64, poll bool) ([]string, error)
	// PollImageResults is _poll_image_results.
	PollImageResults(ctx context.Context, conversationID string, timeoutSecs float64, fileIDs, sedimentIDs []string) ([]string, []string, error)
	// DownloadImages is download_image_bytes.
	DownloadImages(ctx context.Context, urls []string) ([][]byte, error)
	// DeleteConversation hides the conversation in the account's history.
	DeleteConversation(ctx context.Context, conversationID string) error
	Close()
}

// Pipeline holds what every attempt needs.
type Pipeline struct {
	Settings *settings.Store
	Hooks    Hooks
	// Sleep waits (tests replace it); it returns early when the context ends.
	Sleep func(ctx context.Context, d time.Duration)
	// Background runs fire-and-forget work such as removing a conversation.
	Background func(func())
	breaker    streamResetBreaker
	now        func() time.Time
}

// NewPipeline builds a pipeline with real sleeping and goroutines.
func NewPipeline(store *settings.Store, hooks Hooks) *Pipeline {
	p := &Pipeline{Settings: store, Hooks: hooks, now: time.Now}
	p.Sleep = func(ctx context.Context, d time.Duration) {
		select {
		case <-time.After(d):
		case <-ctx.Done():
		}
	}
	p.Background = func(fn func()) { go fn() }
	p.breaker.now = time.Now
	return p
}

func secs(f float64) time.Duration { return time.Duration(f * float64(time.Second)) }

func (p *Pipeline) promptWithGlobalSystem(prompt string) string {
	if system := p.Settings.GlobalSystemPrompt(); system != "" {
		return system + "\n\n" + prompt
	}
	return prompt
}

// detailedErrorFromTasks is _get_detailed_error_from_tasks: the first task error text, or "".
func (p *Pipeline) detailedErrorFromTasks(ctx context.Context, backend Backend, conversationID string, timeout, wait time.Duration) string {
	if wait > 0 {
		p.Sleep(ctx, wait)
	}
	tasks, err := backend.QueryTasks(ctx, conversationID, timeout)
	if err != nil {
		return ""
	}
	for _, task := range tasks {
		if isError, message := backend.CheckTaskError(task); isError && message != "" {
			return message
		}
	}
	return ""
}

// removeConversationLater is _remove_image_conversation_later.
func (p *Pipeline) removeConversationLater(backend Backend, conversationID string, success bool) {
	if conversationID == "" {
		return
	}
	if !(p.Settings.ImageRemoveConversationAlways() || (success && p.Settings.ImageRemoveConversationAfterResult())) {
		return
	}
	p.Background(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		backend.DeleteConversation(ctx, conversationID)
	})
}

func (p *Pipeline) resultOutput(ctx context.Context, backend Backend, req Request, index, total int, urls []string, conversationID string) (Output, bool, error) {
	req.progress("receiving_image")
	images, err := backend.DownloadImages(ctx, urls)
	if err != nil {
		return Output{}, false, err
	}
	items := make([]map[string]any, 0, len(images))
	for _, image := range images {
		items = append(items, map[string]any{"b64_json": base64.StdEncoding.EncodeToString(image)})
	}
	data, _ := p.Hooks.FormatImageResult(items, req.Prompt, req.ResponseFormat, req.BaseURL, time.Now().Unix(), "", req.Size)["data"].([]map[string]any)
	if len(data) == 0 {
		return Output{}, false, nil
	}
	out := newOutput("result", req, index, total)
	out.Data = data
	out.ConversationID = conversationID
	return out, true, nil
}

func (p *Pipeline) recoverConversationID(ctx context.Context, backend Backend, prompt string) string {
	id, err := backend.FindConversationByPrompt(ctx, prompt, p.now(), 5*time.Second)
	if err != nil {
		return ""
	}
	return id
}

func isTransientPollError(err error) bool {
	var timeout *PollTimeoutError
	text := strings.ToLower(err.Error())
	return errors.As(err, &timeout) || IsTLSConnectionError(text) || strings.Contains(text, "upstream") || strings.Contains(text, "connection") || strings.Contains(text, "timeout")
}

// pollRetries is the shared retry poll of the text-reply and fallback paths. waitFirst sleeps
// before each attempt (fallback path); it returns an error only for a failover poll timeout.
func (p *Pipeline) pollRetries(ctx context.Context, backend Backend, req Request, conversationID string, fileIDs, sedimentIDs *[]string, waitFirst bool) error {
	timeout := float64(req.FailoverTimeoutSecs)
	if timeout == 0 {
		timeout = float64(max(p.Settings.ImagePollTimeoutSecs(), 300))
	}
	const attempts = 3
	for attempt := 1; attempt <= attempts; attempt++ {
		if waitFirst {
			p.Sleep(ctx, secs(min(30*float64(attempt), p.Settings.ImagePollInitialWaitSecs()*float64(attempt))))
		}
		polledFiles, polledSediments, err := backend.PollImageResults(ctx, conversationID, timeout, *fileIDs, *sedimentIDs)
		if err == nil {
			*fileIDs = addUnique(*fileIDs, polledFiles...)
			*sedimentIDs = addUnique(*sedimentIDs, polledSediments...)
			return nil
		}
		var pollTimeout *PollTimeoutError
		var policy *ContentPolicyError
		if req.FailoverTimeoutSecs > 0 && errors.As(err, &pollTimeout) {
			return err // 换号生成: let the retry loop switch accounts
		}
		_ = isTransientPollError(err)
		if attempt < attempts && !errors.As(err, &pollTimeout) && !errors.As(err, &policy) {
			p.Sleep(ctx, secs(30*float64(attempt)))
			continue
		}
		break
	}
	return nil
}

// StreamImageOutputs is stream_image_outputs: one attempt with one account. Outputs go to emit;
// the error is what the Python generator raised.
func (p *Pipeline) StreamImageOutputs(ctx context.Context, backend Backend, req Request, index, total int, emit func(Output)) error {
	req = req.withDefaults()
	finalPrompt := p.promptWithGlobalSystem(BuildImagePrompt(req.Prompt, req.Size, req.Quality, req.OutputFormat, req.FrameCount, req.FrameDurationMS))
	payloads, wait, err := backend.StreamPicture(ctx, finalPrompt, req.Model, req.Images, req.Size)
	if err != nil {
		return err
	}
	var last State
	IterConversationPayloads(payloads, "", nil, func(event Event) {
		last = event.State
		switch event.Type {
		case "conversation.delta":
			out := newOutput("progress", req, index, total)
			out.Text = event.Delta
			out.UpstreamEventType = "conversation.delta"
			out.ConversationID = event.State.ConversationID
			emit(out)
		case "conversation.event":
			out := newOutput("progress", req, index, total)
			if raw, ok := event.Raw.(map[string]any); ok {
				out.UpstreamEventType = asString(raw["type"])
			}
			out.ConversationID = event.State.ConversationID
			emit(out)
		}
	})
	if err := wait(); err != nil {
		return err
	}

	conversationID := last.ConversationID
	fileIDs := append([]string(nil), last.FileIDs...)
	sedimentIDs := append([]string(nil), last.SedimentIDs...)
	message := strings.TrimSpace(last.Text)
	req.progress("image_stream_resolve_start")
	messageOut := func(text string) {
		out := newOutput("message", req, index, total)
		out.Text = text
		out.ConversationID = conversationID
		emit(out)
	}

	if message != "" && len(fileIDs) == 0 && len(sedimentIDs) == 0 && last.Blocked {
		detailed := p.detailedErrorFromTasks(ctx, backend, conversationID, 10*time.Second, 2*time.Second)
		text := firstNonEmpty(detailed, message, "Image generation was rejected by upstream policy.")
		messageOut(text)
		return nil
	}
	shouldPoll := len(req.Images) > 0 || last.TurnUseCase == "image gen"
	if message != "" && len(fileIDs) == 0 && len(sedimentIDs) == 0 && !shouldPoll {
		messageOut(message)
		return nil
	}
	isTextReply := message != "" && IsModelTextReplyInsteadOfImage(message)
	if isTextReply && conversationID == "" {
		if id := p.recoverConversationID(ctx, backend, req.Prompt); id != "" {
			conversationID = id
		}
	}
	if len(fileIDs) == 0 && len(sedimentIDs) == 0 && conversationID != "" {
		detailed := p.detailedErrorFromTasks(ctx, backend, conversationID, 5*time.Second, time.Second)
		if detailed != "" && !shouldPoll && !isTextReply {
			messageOut(detailed)
			return nil
		}
	}

	pollTimeout := float64(firstPositive(req.FailoverTimeoutSecs, req.PollTimeoutSecs, p.Settings.ImagePollTimeoutSecs()))
	if isTextReply && conversationID != "" && req.PollTimeoutSecs == 0 && req.FailoverTimeoutSecs == 0 {
		pollTimeout = max(pollTimeout, 300)
	}
	urls, err := backend.ResolveImageURLs(ctx, conversationID, fileIDs, sedimentIDs, pollTimeout, true)
	if err != nil {
		var policy *ContentPolicyError
		var timeout *PollTimeoutError
		switch {
		case isTextReply && errors.As(err, &policy):
			urls = nil // the image may still be generating in the background
		case errors.As(err, &policy) || errors.As(err, &timeout):
			return err
		case isTextReply && conversationID != "":
			urls = nil // a first-poll network error is not final for a text reply
		default:
			return err
		}
	}
	if len(urls) > 0 {
		out, ok, err := p.resultOutput(ctx, backend, req, index, total, urls, conversationID)
		if err != nil {
			return err
		}
		if ok {
			emit(out)
		}
		return nil
	}

	if message != "" {
		if isTextReply && conversationID == "" {
			if id := p.recoverConversationID(ctx, backend, req.Prompt); id != "" {
				conversationID = id
			}
		}
		if isTextReply && conversationID != "" {
			if err := p.pollRetries(ctx, backend, req, conversationID, &fileIDs, &sedimentIDs, false); err != nil {
				return err
			}
			if len(fileIDs) > 0 || len(sedimentIDs) > 0 {
				urls, err := backend.ResolveImageURLs(ctx, conversationID, fileIDs, sedimentIDs, 0, false)
				if err != nil {
					return err
				}
				if len(urls) > 0 {
					out, ok, err := p.resultOutput(ctx, backend, req, index, total, urls, conversationID)
					if err != nil {
						return err
					}
					if ok {
						emit(out)
						return nil
					}
				}
			}
		}
		messageOut(message)
		return nil
	}

	if shouldPoll && conversationID == "" {
		if id := p.recoverConversationID(ctx, backend, req.Prompt); id != "" {
			conversationID = id
		}
	}
	switch {
	case shouldPoll && conversationID != "":
		if err := p.pollRetries(ctx, backend, req, conversationID, &fileIDs, &sedimentIDs, true); err != nil {
			return err
		}
		if len(fileIDs) > 0 || len(sedimentIDs) > 0 {
			urls, err := backend.ResolveImageURLs(ctx, conversationID, fileIDs, sedimentIDs, 0, false)
			if err != nil {
				return err
			}
			if len(urls) > 0 {
				out, ok, err := p.resultOutput(ctx, backend, req, index, total, urls, conversationID)
				if err != nil {
					return err
				}
				if ok {
					emit(out)
					return nil
				}
			}
		}
		messageOut("Image generation completed upstream but the result could not be retrieved. The image may still be processing. Please try again in a moment.")
	case message != "":
		messageOut(message)
	default:
		messageOut("Image generation started upstream but the response was incomplete. Please try again.")
	}
	return nil
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func firstPositive(values ...int) int {
	for _, value := range values {
		if value > 0 {
			return value
		}
	}
	return 0
}
