package pool

import (
	"context"
)

// LoginFunc is the password login (openailogin.Login.PasswordLogin): a result dict with ok,
// error, detail and the tokens.
type LoginFunc func(ctx context.Context, email, password, totpSecret, proxy string) (map[string]any, error)

// ApplyRefreshedTokens is _apply_refreshed_tokens: store new tokens; returns the token now in use.
func (s *Service) ApplyRefreshedTokens(oldToken string, data map[string]string, event string) string {
	return s.applyRefreshedTokens(oldToken, data, event)
}

func loginTokens(result map[string]any) map[string]string {
	return map[string]string{"access_token": str(result["access_token"]), "refresh_token": str(result["refresh_token"]), "id_token": str(result["id_token"])}
}

// loginErrorMessage is the message of a failed login result (error.message, error.code or the error code).
func loginErrorMessage(result map[string]any) string {
	code := str(result["error"])
	if detail, ok := result["detail"].(map[string]any); ok {
		switch inner := detail["error"].(type) {
		case map[string]any:
			if message := str(inner["message"]); message != "" {
				return message
			}
			if innerCode := str(inner["code"]); innerCode != "" {
				return innerCode
			}
		case nil:
		default:
			if text := str(inner); text != "" {
				return text
			}
		}
	}
	return code
}

// PasswordRelogin is _password_re_login_thread as a Relogin: log in again with the stored
// password (and TOTP secret) through the account's proxy and store the new tokens; a deactivated
// account is disabled, any other failure marks it abnormal (or removes it).
func (s *Service) PasswordRelogin(login LoginFunc) Relogin {
	return func(token, email, password, event, progressID, totpSecret string) {
		account := s.GetAccount(token)
		result, err := login(context.Background(), email, password, totpSecret, str(account["proxy"]))
		if err != nil {
			s.logAdd("更新账号", map[string]any{"source": event, "token": AnonymizeToken(token), "email": email, "status": "异常", "error": err.Error()})
			s.RemoveInvalidToken(token, event+":password_relogin_exception", true, err.Error())
			if progressID != "" {
				s.UpdateReloginProgress(progressID, token, "异常", err.Error())
			}
			return
		}
		if result["ok"] == true {
			newToken := s.ApplyRefreshedTokens(token, loginTokens(result), event+":password_relogin")
			sourceType := str(result["source_type"])
			if sourceType == "" {
				sourceType = "password"
			}
			s.UpdateAccount(newToken, Account{"source_type": sourceType, "status": StatusNormal}, true)
			s.logAdd("更新账号", map[string]any{"source": event, "old_token": AnonymizeToken(token), "new_token": AnonymizeToken(str(result["access_token"])),
				"email": email, "status": "成功"})
			if progressID != "" {
				s.UpdateReloginProgress(progressID, token, "成功", "")
			}
			return
		}
		code := str(result["error"])
		detail := result["detail"]
		if detail == nil {
			detail = map[string]any{}
		}
		s.logAdd("更新账号", map[string]any{"source": event, "token": AnonymizeToken(token), "email": email, "status": "失败", "error": code, "detail": detail})
		if inner, _ := detailMap(result)["error"].(map[string]any); code == "password_verify_failed_403" && str(inner["code"]) == "account_deactivated" {
			s.UpdateAccount(token, Account{"status": StatusDisabled, "quota": 0}, true)
			s.logAdd("账号已停用-标记禁用", map[string]any{"source": event, "token": AnonymizeToken(token), "email": email, "detail": detail})
			if progressID != "" {
				s.UpdateReloginProgress(progressID, token, StatusDisabled, "")
			}
			return
		}
		s.RemoveInvalidToken(token, event+":password_relogin_failed", true, loginErrorMessage(result))
		if progressID != "" {
			s.UpdateReloginProgress(progressID, token, StatusAbnormal, code)
		}
	}
}

func detailMap(result map[string]any) map[string]any {
	detail, _ := result["detail"].(map[string]any)
	if detail == nil {
		return map[string]any{}
	}
	return detail
}
