package imagegen

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/basketikun/chatgpt2api/internal/pool"
)

const (
	maxTextReplyRetries          = 3
	maxTLSRetries                = 3
	maxConnTimeoutRetries        = 3
	maxPollTimeoutRetries        = 4
	minFailoverSecs              = 15
	maxEditUploadThrottleRetries = 5
	maxForbiddenRetries          = 3
)

// Accounts is the part of the account pool the retry loop uses.
type Accounts interface {
	GetAvailableAccessToken(ctx context.Context, filter pool.Filter, forEdits bool) (string, error)
	GetAccount(token string) pool.Account
	MarkImageResult(token string, success bool) pool.Account
	ReleaseImageSlot(token string)
	SidelineImageAccount(token string, d time.Duration)
	SameProxyTokens(token string) map[string]bool
	RefreshAccessToken(ctx context.Context, token string, force bool, event string) string
	RemoveInvalidToken(token, event string, quiet bool, cause string) bool
	MarkEditUploadCooldown(token string, cause any) string
}

// Generator runs whole image requests: account selection, attempts and retries.
type Generator struct {
	*Pipeline
	Accounts Accounts
	// NewBackend opens a ChatGPT client for one account.
	NewBackend func(token string, account pool.Account) (Backend, error)
	// Codex runs one attempt on the Codex route (stream_codex_image_outputs); nil = not available.
	Codex func(ctx context.Context, token string, account pool.Account, req Request, index, total int, emit func(Output)) error
	// Stream replaces StreamImageOutputs for one web attempt (tests mock it like the Python tests do).
	Stream func(ctx context.Context, backend Backend, req Request, index, total int, emit func(Output)) error
}

func accountEmail(account pool.Account) string {
	email, _ := account["email"].(string)
	return strings.TrimSpace(email)
}

func withAccount(err *GenerationError, email string) *GenerationError {
	if email != "" && err.AccountEmail == "" {
		err.AccountEmail = email
	}
	return err
}

// attemptResult says how one attempt ended.
type attemptResult struct {
	outputs  []Output
	emitted  bool
	message  bool
	result   bool
	lastConv string
}

func (g *Generator) attempt(ctx context.Context, token string, account pool.Account, req Request, index, total int, email string) (attemptResult, error) {
	var res attemptResult
	backend, err := g.NewBackend(token, account)
	if err != nil {
		return res, err
	}
	defer backend.Close()
	var raised error
	emit := func(out Output) {
		if raised != nil {
			return
		}
		res.lastConv = firstNonEmpty(out.ConversationID, res.lastConv)
		if email != "" && out.AccountEmail == "" {
			out.AccountEmail = email
		}
		if out.Kind == "message" && req.MessageAsError {
			raised = &GenerationError{Message: firstNonEmpty(out.Text, "Image generation was rejected by upstream policy."), StatusCode: 400,
				ErrorType: "invalid_request_error", Code: "content_policy_violation", AccountEmail: email, ConversationID: out.ConversationID}
			return
		}
		res.emitted = true
		res.message = out.Kind == "message"
		res.result = res.result || out.Kind == "result"
		res.outputs = append(res.outputs, out)
	}
	if IsCodexImageModel(req.Model) {
		if g.Codex == nil {
			err = NewGenerationError("the Codex image route is not available")
		} else {
			err = g.Codex(ctx, token, account, req, index, total, emit)
		}
	} else if g.Stream != nil {
		err = g.Stream(ctx, backend, req, index, total, emit)
	} else {
		err = g.StreamImageOutputs(ctx, backend, req, index, total, emit)
	}
	if raised != nil {
		err = raised
	}
	if err != nil {
		var policy *ContentPolicyError
		var timeout *PollTimeoutError
		var generation *GenerationError
		switch {
		case errors.As(err, &policy):
			res.lastConv = firstNonEmpty(res.lastConv, policy.ConversationID)
		case errors.As(err, &timeout):
			res.lastConv = firstNonEmpty(res.lastConv, timeout.ConversationID)
		case errors.As(err, &generation):
			res.lastConv = firstNonEmpty(res.lastConv, generation.ConversationID)
		}
	}
	g.removeConversationLater(backend, res.lastConv, res.result)
	return res, err
}

// GenerateSingleImage is _generate_single_image: pick an account, run one attempt, and on failure
// decide whether to switch accounts. Every path gives the account's slot back.
func (g *Generator) GenerateSingleImage(ctx context.Context, req Request, index, total int) ([]Output, error) {
	req = req.withDefaults()
	var (
		textReplyRetries, tlsRetries, connTimeoutRetries, pollTimeoutRetries    int
		editUploadRetries, modelNotFoundRetries, forbiddenRetries, resetRetries int
	)
	triedUpload := map[string]bool{}
	avoided := map[string]bool{}
	failoverEnabled := g.Settings.ImageFailoverEnabled() && req.PollTimeoutSecs == 0
	failoverBudget := g.Settings.ImagePollTimeoutSecs()
	forEdits := len(req.Images) > 0
	email := ""
	plan, _ := SplitImageModel(req.Model)
	codex := IsCodexImageModel(req.Model)

	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		req.progress("getting_account")
		filter := pool.Filter{PlanType: plan, Excluded: map[string]bool{}}
		if codex {
			filter.SourceType = "codex"
			if plan == "" {
				filter.PlanTypes = []string{"plus", "team", "pro"}
			}
		}
		for token := range triedUpload {
			filter.Excluded[token] = true
		}
		for token := range avoided {
			filter.Excluded[token] = true
		}
		token, err := g.Accounts.GetAvailableAccessToken(ctx, filter, forEdits)
		if err != nil {
			if len(avoided) > 0 {
				avoided = map[string]bool{} // no account on another egress: fall back to the same one
				continue
			}
			message := err.Error()
			if message == "" {
				message = "image generation failed"
			}
			return nil, withAccount(NewGenerationError(message), email)
		}

		attemptReq := req
		attemptWait := 0
		if failoverEnabled {
			attemptWait = max(1, min(g.Settings.ImageFailoverSecs(), failoverBudget))
			attemptReq.FailoverTimeoutSecs = attemptWait
		}
		account := g.Accounts.GetAccount(token)
		email = accountEmail(account)
		res, err := g.attempt(ctx, token, account, attemptReq, index, total, email)
		if err == nil {
			switch {
			case res.message:
				g.Accounts.MarkImageResult(token, false)
				return res.outputs, nil
			case !res.result:
				g.Accounts.MarkImageResult(token, false)
				if res.emitted {
					conv := ""
					if len(res.outputs) > 0 {
						conv = res.outputs[len(res.outputs)-1].ConversationID
					}
					return nil, &GenerationError{Message: "upstream completed without generating images", StatusCode: 400,
						ErrorType: "invalid_request_error", Code: "no_image_generated", AccountEmail: email, ConversationID: conv}
				}
				return res.outputs, nil
			}
			g.Accounts.MarkImageResult(token, true)
			return res.outputs, nil
		}

		var timeout *PollTimeoutError
		var policy *ContentPolicyError
		var generation *GenerationError
		switch {
		case errors.As(err, &timeout):
			if failoverEnabled {
				// 换号生成: the conversation got stuck, which is not the account's fault
				failoverBudget -= attemptWait
				if !(res.message || res.result) && failoverBudget >= minFailoverSecs && pollTimeoutRetries < maxPollTimeoutRetries {
					pollTimeoutRetries++
					avoided[token] = true
					g.Accounts.ReleaseImageSlot(token) // no result recorded, but the slot must come back
					continue
				}
				g.Accounts.MarkImageResult(token, false)
				return nil, &PollTimeoutError{
					Message: fmt.Sprintf("ChatGPT 生图超时：换了 %d 个账号，每个最多等 %d 秒都没有出图，上游可能正拥堵，请稍后重试。",
						pollTimeoutRetries+1, g.Settings.ImageFailoverSecs()),
					ConversationID: timeout.ConversationID, AccountEmail: email,
				}
			}
			g.Accounts.MarkImageResult(token, false)
			timeout.AccountEmail = firstNonEmpty(timeout.AccountEmail, email)
			if !res.emitted {
				pollTimeoutRetries++
				if pollTimeoutRetries <= maxPollTimeoutRetries {
					continue
				}
			}
			return nil, timeout

		case errors.As(err, &policy):
			g.Accounts.MarkImageResult(token, false)
			return nil, &GenerationError{Message: firstNonEmpty(policy.Message, "Image generation was rejected by upstream policy."), StatusCode: 400,
				ErrorType: "invalid_request_error", Code: "content_policy_violation", AccountEmail: email, ConversationID: policy.ConversationID}

		case errors.As(err, &generation):
			g.Accounts.MarkImageResult(token, false)
			withAccount(generation, email)
			if IsModelTextReplyInsteadOfImage(generation.Message) && !res.emitted {
				textReplyRetries++
				if textReplyRetries <= maxTextReplyRetries {
					continue
				}
				return nil, &GenerationError{Message: "Image generation failed: the upstream model returned a text description instead of generating an image. Please try again later.",
					StatusCode: 502, ErrorType: "server_error", Code: "upstream_text_reply", AccountEmail: email, ConversationID: generation.ConversationID}
			}
			return nil, generation
		}

		// any other failure
		g.Accounts.MarkImageResult(token, false)
		last := err.Error()
		if !res.emitted && IsImageModelNotFound(err) {
			g.Accounts.SidelineImageAccount(token, ModelNotFoundCooldown)
			modelNotFoundRetries++
			if modelNotFoundRetries <= maxPollTimeoutRetries {
				continue
			}
		}
		if !res.emitted && IsTokenInvalidError(last) {
			if refreshed := g.Accounts.RefreshAccessToken(ctx, token, true, "image_stream"); refreshed != "" && refreshed != token {
				continue
			}
			g.Accounts.RemoveInvalidToken(token, "image_stream", false, last)
			continue
		}
		if IsUpstreamForbidden(err) {
			// 403 is usually Cloudflare checking the egress IP: cool the account, avoid its egress this time
			g.Accounts.SidelineImageAccount(token, ForbiddenCooldown)
			forbiddenRetries++
			if !res.emitted && forbiddenRetries <= maxForbiddenRetries {
				for other := range g.Accounts.SameProxyTokens(token) {
					avoided[other] = true
				}
				continue
			}
		}
		if !res.emitted && IsStreamResetError(last) {
			open := g.breaker.note()
			resetRetries++
			if !open && resetRetries <= maxStreamResetRetries {
				avoided[token] = true
				continue
			}
		}
		if !res.emitted && IsTLSConnectionError(last) {
			tlsRetries++
			if tlsRetries <= maxTLSRetries {
				g.Sleep(ctx, time.Duration(min(2*tlsRetries, 10))*time.Second)
				continue
			}
		}
		if !res.emitted && IsConnectionTimeoutError(last) {
			connTimeoutRetries++
			if connTimeoutRetries <= maxConnTimeoutRetries {
				g.Sleep(ctx, time.Duration(min(3*connTimeoutRetries, 9))*time.Second)
				continue
			}
		}
		if forEdits && g.Settings.ImageEditUploadCooldownEnabled() && !res.emitted && pool.IsEditUploadThrottled(last) {
			g.Accounts.MarkEditUploadCooldown(token, last)
			triedUpload[token] = true
			editUploadRetries++
			if editUploadRetries <= maxEditUploadThrottleRetries {
				continue
			}
		}
		return nil, &GenerationError{Message: ImageStreamErrorMessage(last), StatusCode: 502, ErrorType: "server_error", Code: "upstream_error", AccountEmail: email}
	}
}

// StreamWithPool is stream_image_outputs_with_pool: n images in parallel (or one after another),
// successful images first; an error only when none succeeded.
func (g *Generator) StreamWithPool(ctx context.Context, req Request) ([]Output, error) {
	req = req.withDefaults()
	if !IsSupportedImageModel(req.Model) {
		return nil, NewGenerationError("unsupported image model,supported models: " + strings.Join(ImageModels(), ", "))
	}
	if strings.ToLower(req.OutputFormat) == "gif" && len(req.Images) > 0 {
		return nil, NewGenerationError("GIF output is not supported for image edits")
	}
	if req.N <= 1 {
		return g.GenerateSingleImage(ctx, req, 1, 1)
	}
	results := make([][]Output, req.N+1)
	errs := make([]error, req.N+1)
	if !g.Settings.ImageParallelGeneration() {
		out := []Output{}
		for index := 1; index <= req.N; index++ {
			outputs, err := g.GenerateSingleImage(ctx, req, index, req.N)
			if err != nil {
				return nil, err // serial mode stops at the first failure, like the Python generator
			}
			out = append(out, outputs...)
		}
		return out, nil
	}
	var wg sync.WaitGroup
	for index := 1; index <= req.N; index++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			results[index], errs[index] = g.GenerateSingleImage(ctx, req, index, req.N)
		}(index)
	}
	wg.Wait()
	out := []Output{}
	last := ""
	for index := 1; index <= req.N; index++ {
		if errs[index] != nil {
			last = errs[index].Error()
			continue
		}
		out = append(out, results[index]...)
	}
	if len(out) == 0 {
		if last == "" {
			last = "no account in the pool could generate images — check account quota and rate-limit status"
		}
		return nil, NewGenerationError(ImageStreamErrorMessage(last))
	}
	return out, nil
}
