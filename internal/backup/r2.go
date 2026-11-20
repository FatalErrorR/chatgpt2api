package backup

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// R2 is CloudflareR2Client: S3 API with AWS Signature V4 (region "auto").
type R2 struct {
	AccountID, AccessKeyID, SecretAccessKey, Bucket, Prefix string
	Endpoint                                                string // https://<account>.r2.cloudflarestorage.com unless set (tests)
	Client                                                  *http.Client
	Now                                                     func() time.Time
}

func newR2(settings map[string]any) *R2 {
	prefix := clean(settings["prefix"])
	if prefix == "" {
		prefix = "backups"
	}
	return &R2{AccountID: clean(settings["account_id"]), AccessKeyID: clean(settings["access_key_id"]),
		SecretAccessKey: clean(settings["secret_access_key"]), Bucket: clean(settings["bucket"]), Prefix: prefix,
		Client: &http.Client{Timeout: 60 * time.Second}, Now: time.Now}
}

// Validate is validate(): the four required settings.
func (r *R2) Validate() error {
	missing := []string{}
	for _, field := range []struct{ value, label string }{{r.AccountID, "Account ID"}, {r.AccessKeyID, "Access Key ID"},
		{r.SecretAccessKey, "Secret Access Key"}, {r.Bucket, "Bucket"}} {
		if field.value == "" {
			missing = append(missing, field.label)
		}
	}
	if len(missing) > 0 {
		return &Error{"R2 配置不完整：缺少 " + strings.Join(missing, "、")}
	}
	return nil
}

func (r *R2) endpoint() string {
	if r.Endpoint != "" {
		return strings.TrimRight(r.Endpoint, "/")
	}
	return "https://" + r.AccountID + ".r2.cloudflarestorage.com"
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func hmacSHA256(key []byte, message string) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(message))
	return mac.Sum(nil)
}

// encodeQuery is urlencode(sorted(query.items())).
func encodeQuery(query map[string]string) string {
	keys := make([]string, 0, len(query))
	for key := range query {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := []string{}
	for _, key := range keys {
		parts = append(parts, url.QueryEscape(key)+"="+url.QueryEscape(query[key]))
	}
	return strings.Join(parts, "&")
}

// quotePath is quote(key, safe="/").
func quotePath(key string) string {
	segments := strings.Split(key, "/")
	for i, segment := range segments {
		segments[i] = strings.ReplaceAll(url.PathEscape(segment), "+", "%2B")
	}
	return strings.Join(segments, "/")
}

// sign is _aws_v4_headers: the encoded query and the request headers with the authorization.
func (r *R2) sign(method, objectPath string, query map[string]string, body []byte, extra map[string]string, host string) (string, map[string]string) {
	now := r.Now().UTC()
	amzDate, dateStamp := now.Format("20060102T150405Z"), now.Format("20060102")
	encodedQuery := encodeQuery(query)
	payloadHash := sha256Hex(body)
	headers := map[string]string{"host": host, "x-amz-content-sha256": payloadHash, "x-amz-date": amzDate}
	for k, v := range extra {
		headers[strings.ToLower(k)] = strings.TrimSpace(v)
	}
	names := make([]string, 0, len(headers))
	for k := range headers {
		names = append(names, k)
	}
	sort.Strings(names)
	var canonicalHeaders strings.Builder
	for _, name := range names {
		canonicalHeaders.WriteString(name + ":" + strings.Join(strings.Fields(headers[name]), " ") + "\n")
	}
	signedHeaders := strings.Join(names, ";")
	canonical := strings.Join([]string{strings.ToUpper(method), objectPath, encodedQuery, canonicalHeaders.String(), signedHeaders, payloadHash}, "\n")
	scope := dateStamp + "/auto/s3/aws4_request"
	toSign := strings.Join([]string{"AWS4-HMAC-SHA256", amzDate, scope, sha256Hex([]byte(canonical))}, "\n")
	signing := hmacSHA256(hmacSHA256(hmacSHA256(hmacSHA256([]byte("AWS4"+r.SecretAccessKey), dateStamp), "auto"), "s3"), "aws4_request")
	headers["authorization"] = fmt.Sprintf("AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s", r.AccessKeyID, scope, signedHeaders,
		hex.EncodeToString(hmacSHA256(signing, toSign)))
	return encodedQuery, headers
}

func (r *R2) do(method, key string, query map[string]string, body []byte, extra map[string]string) (*http.Response, []byte, error) {
	objectPath := "/" + r.Bucket
	if key != "" {
		objectPath += "/" + quotePath(strings.TrimLeft(key, "/"))
	}
	host := r.AccountID + ".r2.cloudflarestorage.com"
	if r.Endpoint != "" {
		if parsed, err := url.Parse(r.Endpoint); err == nil {
			host = parsed.Host
		}
	}
	encodedQuery, headers := r.sign(method, objectPath, query, body, extra, host)
	target := r.endpoint() + objectPath
	if encodedQuery != "" {
		target += "?" + encodedQuery
	}
	request, err := http.NewRequest(strings.ToUpper(method), target, bytes.NewReader(body))
	if err != nil {
		return nil, nil, err
	}
	for name, value := range headers {
		if name != "host" {
			request.Header.Set(name, value)
		}
	}
	response, err := r.Client.Do(request)
	if err != nil {
		return nil, nil, err
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(response.Body)
	return response, raw, err
}

// Test is test_connection.
func (r *R2) Test() (map[string]any, error) {
	if err := r.Validate(); err != nil {
		return nil, err
	}
	response, _, err := r.do(http.MethodGet, "", map[string]string{"list-type": "2", "max-keys": "1"}, nil, nil)
	if err != nil {
		return nil, err
	}
	if response.StatusCode >= 400 {
		return nil, &Error{fmt.Sprintf("连接 R2 失败：HTTP %d", response.StatusCode)}
	}
	return map[string]any{"ok": true, "status": response.StatusCode}, nil
}

// Upload is upload_bytes.
func (r *R2) Upload(key string, payload []byte, contentType string, metadata map[string]string) (string, error) {
	headers := map[string]string{"content-type": contentType}
	for k, v := range metadata {
		headers["x-amz-meta-"+k] = v
	}
	response, _, err := r.do(http.MethodPut, key, nil, payload, headers)
	if err != nil {
		return "", err
	}
	if response.StatusCode >= 400 {
		return "", &Error{fmt.Sprintf("上传备份失败：HTTP %d", response.StatusCode)}
	}
	return key, nil
}

// Delete is delete_object (a missing object is fine).
func (r *R2) Delete(key string) error {
	response, _, err := r.do(http.MethodDelete, key, nil, nil, nil)
	if err != nil {
		return err
	}
	if response.StatusCode >= 400 && response.StatusCode != 404 {
		return &Error{fmt.Sprintf("删除备份失败：HTTP %d", response.StatusCode)}
	}
	return nil
}

// Download is download_bytes.
func (r *R2) Download(key string) ([]byte, error) {
	response, raw, err := r.do(http.MethodGet, key, nil, nil, nil)
	if err != nil {
		return nil, err
	}
	if response.StatusCode >= 400 {
		return nil, &Error{fmt.Sprintf("读取备份失败：HTTP %d", response.StatusCode)}
	}
	return raw, nil
}

func between(text, start, end string) string {
	_, after, ok := strings.Cut(text, start)
	if !ok {
		return ""
	}
	value, _, _ := strings.Cut(after, end)
	return strings.TrimSpace(value)
}

// List is list_objects: every object under the prefix, newest first.
func (r *R2) List() ([]map[string]any, error) {
	items := []map[string]any{}
	continuation := ""
	for {
		query := map[string]string{"list-type": "2", "prefix": strings.TrimRight(r.Prefix, "/") + "/", "max-keys": "1000"}
		if continuation != "" {
			query["continuation-token"] = continuation
		}
		response, raw, err := r.do(http.MethodGet, "", query, nil, nil)
		if err != nil {
			return nil, err
		}
		if response.StatusCode >= 400 {
			return nil, &Error{fmt.Sprintf("获取备份列表失败：HTTP %d", response.StatusCode)}
		}
		text := string(raw)
		for _, block := range strings.Split(text, "<Contents>")[1:] {
			key := between(block, "<Key>", "</Key>")
			if key == "" {
				continue
			}
			size, _ := strconv.Atoi(between(block, "<Size>", "</Size>"))
			items = append(items, map[string]any{"key": key, "size": size, "updated_at": between(block, "<LastModified>", "</LastModified>")})
		}
		if !strings.Contains(text, "<IsTruncated>true</IsTruncated>") {
			break
		}
		if continuation = between(text, "<NextContinuationToken>", "</NextContinuationToken>"); continuation == "" {
			break
		}
	}
	sort.SliceStable(items, func(i, j int) bool { return items[i]["updated_at"].(string) > items[j]["updated_at"].(string) })
	return items, nil
}
