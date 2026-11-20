package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"
	"sync"

	http "github.com/bogdanfinn/fhttp"
)

// getJSON calls one backend endpoint and decodes the JSON body with numbers kept as json.Number.
// Errors read "<path> failed: HTTP <status>" like the Python client; 401 is ErrTokenInvalidated.
func (u *OpenAIBackendAPI) getJSON(ctx context.Context, method, path string, payload any) (map[string]any, error) {
	response, err := u.request(ctx, method, path, payload, nil)
	if err != nil {
		var httpErr *UpstreamHTTPError
		if errors.As(err, &httpErr) {
			return nil, fmt.Errorf("%s failed: HTTP %d", strings.SplitN(path, "?", 2)[0], httpErr.StatusCode)
		}
		if errors.Is(err, ErrTokenInvalidated) {
			return nil, fmt.Errorf("token invalidated (%s): %w", strings.SplitN(path, "?", 2)[0], ErrTokenInvalidated)
		}
		return nil, err
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(response.Body)
	if err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var data map[string]any
	if err := decoder.Decode(&data); err != nil {
		return nil, err
	}
	if data == nil {
		data = map[string]any{}
	}
	return data, nil
}

func objectAt(value any, keys ...string) map[string]any {
	current, _ := value.(map[string]any)
	for _, key := range keys {
		if current == nil {
			return map[string]any{}
		}
		current, _ = current[key].(map[string]any)
	}
	if current == nil {
		return map[string]any{}
	}
	return current
}

// UserInfo is the Python OpenAIBackendAPI.get_user_info: /backend-api/me, /conversation/init and
// /accounts/check in parallel, giving email, user_id, plan type, image quota and restore time.
func (u *OpenAIBackendAPI) UserInfo(ctx context.Context) (map[string]any, error) {
	if u.accessToken == "" {
		return nil, errors.New("access_token is required")
	}
	var (
		wg                 sync.WaitGroup
		me, init, accounts map[string]any
		errs               [3]error
	)
	wg.Add(3)
	go func() {
		defer wg.Done()
		me, errs[0] = u.getJSON(ctx, http.MethodGet, "/backend-api/me", nil)
	}()
	go func() {
		defer wg.Done()
		init, errs[1] = u.getJSON(ctx, http.MethodPost, "/backend-api/conversation/init", map[string]any{
			"gizmo_id": nil, "requested_default_model": nil, "conversation_id": nil, "timezone_offset_min": -480,
		})
	}()
	go func() {
		defer wg.Done()
		accounts, errs[2] = u.getJSON(ctx, http.MethodGet, "/backend-api/accounts/check/v4-2023-04-27?"+url.Values{"timezone_offset_min": {"-480"}}.Encode(), nil)
	}()
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			return nil, err
		}
	}
	defaultAccount := objectAt(accounts, "accounts", "default", "account")
	plan := "free"
	if value, ok := defaultAccount["plan_type"].(string); ok && value != "" {
		plan = value
	}
	limits, _ := init["limits_progress"].([]any)
	if limits == nil {
		limits = []any{}
	}
	quota, restoreAt := 0, any(nil)
	for _, item := range limits {
		entry, _ := item.(map[string]any)
		if entry == nil || entry["feature_name"] != "image_gen" {
			continue
		}
		if n, ok := entry["remaining"].(json.Number); ok {
			if value, err := n.Int64(); err == nil {
				quota = int(value)
			} else if f, err := n.Float64(); err == nil {
				quota = int(f)
			}
		}
		if reset, ok := entry["reset_after"].(string); ok && reset != "" {
			restoreAt = reset
		}
		break
	}
	status := "正常"
	if quota == 0 {
		status = "限流"
	}
	return map[string]any{
		"email": me["email"], "user_id": me["id"], "type": plan, "quota": quota, "limits_progress": limits,
		"default_model_slug": init["default_model_slug"], "restore_at": restoreAt, "status": status,
	}, nil
}
