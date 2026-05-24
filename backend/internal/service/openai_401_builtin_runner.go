package service

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	mathrand "math/rand/v2"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/cookiejar"
	"net/mail"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/proxyurl"
	"github.com/Wei-Shaw/sub2api/internal/pkg/proxyutil"
)

const (
	openAI401AuthBaseURL      = "https://auth.openai.com"
	openAI401ChatGPTBaseURL   = "https://chatgpt.com"
	openAI401SentinelReqURL   = "https://sentinel.openai.com/backend-api/sentinel/req"
	openAI401SentinelSDKURL   = "https://sentinel.openai.com/sentinel/20260124ceb8/sdk.js"
	openAI401DefaultUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/145.0.0.0 Safari/537.36"
)

type builtinOpenAI401ReloginRunner struct{}

func (builtinOpenAI401ReloginRunner) Run(ctx context.Context, account *Account, cfg config.TokenRelogin401Config) (map[string]any, error) {
	if account == nil {
		return nil, errors.New("account is nil")
	}
	email := openAI401ReloginEmail(account)
	if email == "" {
		return nil, errors.New("account email is required for built-in relogin")
	}

	client, err := newOpenAI401HTTPClient(account.GetCredential("proxy_url"))
	if err != nil {
		return nil, err
	}
	login := &openAI401ProtocolLogin{
		client: client,
		email:  email,
	}

	if sessionToken := openAI401CredentialString(account, "session_token", "_session_token", "chatgpt_session_token"); sessionToken != "" {
		payload, err := login.refreshFromSessionToken(ctx, sessionToken)
		if err == nil {
			return payload, nil
		}
		slog.Warn("openai_401_relogin.builtin_session_refresh_failed", "account_id", account.ID, "error", err)
	}

	if cfg.TempEmailBaseURL == "" || cfg.TempEmailAdminAuth == "" {
		return nil, errors.New("built-in provider requires Cloudflare Temp Email base URL and admin auth")
	}
	password := openAI401CredentialString(account, "password", "login_password", "account_password", "openai_password")

	mailbox := newCloudflareTempEmailMailbox(cfg.TempEmailBaseURL, cfg.TempEmailAdminAuth, client)
	payload, err := login.run(ctx, password, mailbox)
	if err != nil {
		return nil, err
	}
	return payload, nil
}

func newOpenAI401HTTPClient(proxy string) (*http.Client, error) {
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, err
	}
	transport, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return nil, fmt.Errorf("unexpected default transport %T", http.DefaultTransport)
	}
	cloned := transport.Clone()
	if proxy = strings.TrimSpace(proxy); proxy != "" {
		_, parsed, err := proxyurl.Parse(proxy)
		if err != nil {
			return nil, fmt.Errorf("parse proxy URL: %w", err)
		}
		if err := proxyutil.ConfigureTransportProxy(cloned, parsed); err != nil {
			return nil, fmt.Errorf("configure proxy: %w", err)
		}
	}
	return &http.Client{
		Timeout:   35 * time.Second,
		Jar:       jar,
		Transport: cloned,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}, nil
}

type openAI401ProtocolLogin struct {
	client         *http.Client
	email          string
	authBaseURL    string
	chatGPTBaseURL string
	sessionToken   string
	deviceID       string
	csrfToken      string
	codeChallenge  string
	clientID       string
	redirectURI    string
	state          string
	lastSentinel   string
}

func (l *openAI401ProtocolLogin) run(ctx context.Context, password string, mailbox openAI401Mailbox) (map[string]any, error) {
	if l.client == nil {
		return nil, errors.New("http client is nil")
	}
	if mailbox == nil {
		return nil, errors.New("mailbox is nil")
	}
	csrf, err := l.getCSRFToken(ctx)
	if err != nil {
		return nil, err
	}
	authURL, err := l.getAuthURL(ctx, csrf)
	if err != nil {
		return nil, err
	}
	if err := l.initOAuth(ctx, authURL); err != nil {
		return nil, err
	}
	sentinel, err := l.getSentinelToken(ctx, "authorize_continue")
	if err != nil {
		return nil, err
	}
	step, err := l.authorizeContinue(ctx, l.email, sentinel, "login", "https://auth.openai.com/log-in")
	if err != nil {
		return nil, err
	}
	continueURL := extractOpenAI401ContinueURL(step)
	pageType := extractOpenAI401PageType(step)
	if openAI401RequiresPhoneVerification(pageType, continueURL) {
		return nil, fmt.Errorf("OpenAI login requires phone verification: page_type=%s continue_url=%s", pageType, continueURL)
	}

	if openAI401RequiresPassword(pageType, continueURL) {
		if strings.TrimSpace(password) == "" {
			return nil, errors.New("OpenAI login requires password for this account, but no password credential is configured")
		}
		step, err = l.verifyPassword(ctx, password)
		if err != nil {
			return nil, err
		}
		continueURL = extractOpenAI401ContinueURL(step)
		pageType = extractOpenAI401PageType(step)
		if openAI401RequiresPhoneVerification(pageType, continueURL) {
			return nil, fmt.Errorf("OpenAI login requires phone verification: page_type=%s continue_url=%s", pageType, continueURL)
		}
	}

	if continueURL == "" || strings.Contains(continueURL, "/email-verification") || pageType == "email_otp_verification" {
		sentAt := time.Now().Add(-5 * time.Second)
		if err := l.resendOTP(ctx); err != nil {
			if sendErr := l.sendOTP(ctx); sendErr != nil {
				return nil, fmt.Errorf("request email OTP: resend failed: %v; send failed: %w", err, sendErr)
			}
		}
		code, err := mailbox.WaitForOTP(ctx, l.email, sentAt, 180*time.Second)
		if err != nil {
			return nil, err
		}
		step, err = l.verifyOTP(ctx, code)
		if err != nil {
			return nil, err
		}
		continueURL = extractOpenAI401ContinueURL(step)
		pageType = extractOpenAI401PageType(step)
		if openAI401RequiresPhoneVerification(pageType, continueURL) {
			return nil, fmt.Errorf("OpenAI login requires phone verification: page_type=%s continue_url=%s", pageType, continueURL)
		}
	}

	if continueURL == "" {
		continueURL = authURL
	}
	_, _, err = l.followRedirects(ctx, l.normalizeContinueURL(continueURL))
	if err != nil {
		return nil, err
	}
	payload, err := l.getAuthSession(ctx)
	if err != nil {
		return nil, err
	}
	return payload, nil
}

func (l *openAI401ProtocolLogin) refreshFromSessionToken(ctx context.Context, sessionToken string) (map[string]any, error) {
	if strings.TrimSpace(sessionToken) == "" {
		return nil, errors.New("session token is empty")
	}
	l.sessionToken = strings.TrimSpace(sessionToken)
	l.setCookie("chatgpt.com", "__Secure-next-auth.session-token", sessionToken)
	l.setCookieForBaseURL(l.chatGPTBaseURL, "__Secure-next-auth.session-token", sessionToken)
	return l.getAuthSession(ctx)
}

func (l *openAI401ProtocolLogin) authURL(path string) string {
	base := strings.TrimRight(strings.TrimSpace(l.authBaseURL), "/")
	if base == "" {
		base = openAI401AuthBaseURL
	}
	return base + "/" + strings.TrimLeft(path, "/")
}

func (l *openAI401ProtocolLogin) chatGPTURL(path string) string {
	base := strings.TrimRight(strings.TrimSpace(l.chatGPTBaseURL), "/")
	if base == "" {
		base = openAI401ChatGPTBaseURL
	}
	return base + "/" + strings.TrimLeft(path, "/")
}

func (l *openAI401ProtocolLogin) normalizeContinueURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if strings.HasPrefix(raw, "/") {
		return l.authURL(raw)
	}
	return raw
}

func (l *openAI401ProtocolLogin) getCSRFToken(ctx context.Context) (string, error) {
	var payload map[string]any
	if err := l.doJSON(ctx, http.MethodGet, l.chatGPTURL("/api/auth/csrf"), "https://chatgpt.com/auth/login", nil, &payload); err != nil {
		return "", fmt.Errorf("get ChatGPT csrf: %w", err)
	}
	csrf := openAI401String(payload["csrfToken"])
	if csrf == "" {
		return "", errors.New("ChatGPT csrf response missing csrfToken")
	}
	l.csrfToken = csrf
	return csrf, nil
}

func (l *openAI401ProtocolLogin) getAuthURL(ctx context.Context, csrf string) (string, error) {
	form := url.Values{}
	form.Set("csrfToken", csrf)
	form.Set("callbackUrl", "https://chatgpt.com/")
	form.Set("json", "true")
	var payload map[string]any
	if err := l.doFormJSON(ctx, l.chatGPTURL("/api/auth/signin/openai"), "https://chatgpt.com/auth/login", form, &payload); err != nil {
		return "", fmt.Errorf("get OpenAI auth URL: %w", err)
	}
	authURL := openAI401String(payload["url"])
	if authURL == "" {
		return "", errors.New("signin response missing auth URL")
	}
	return l.captureAuthParams(authURL)
}

func (l *openAI401ProtocolLogin) initOAuth(ctx context.Context, authURL string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, authURL, nil)
	if err != nil {
		return err
	}
	applyOpenAI401Headers(req, "https://chatgpt.com/auth/login", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	resp, err := l.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("oauth init HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	if did := l.cookieValue("auth.openai.com", "oai-did"); did != "" {
		l.deviceID = did
	}
	if did := l.cookieValue("chatgpt.com", "oai-did"); l.deviceID == "" && did != "" {
		l.deviceID = did
	}
	if l.deviceID == "" {
		l.deviceID = newOpenAI401UUID()
		l.setCookie("auth.openai.com", "oai-did", l.deviceID)
		l.setCookie("chatgpt.com", "oai-did", l.deviceID)
	}
	return nil
}

func (l *openAI401ProtocolLogin) getSentinelToken(ctx context.Context, flow string) (string, error) {
	token, err := buildOpenAI401SentinelToken(ctx, l.client, l.deviceID, flow)
	if err != nil {
		return "", err
	}
	l.lastSentinel = token
	return token, nil
}

func (l *openAI401ProtocolLogin) authorizeContinue(ctx context.Context, email, sentinel, screenHint, referer string) (map[string]any, error) {
	payload := map[string]any{
		"username":    map[string]any{"value": email, "kind": "email"},
		"screen_hint": screenHint,
	}
	headers := map[string]string{}
	if sentinel != "" {
		headers["openai-sentinel-token"] = sentinel
	}
	var out map[string]any
	if err := l.doJSON(ctx, http.MethodPost, l.authURL("/api/accounts/authorize/continue"), referer, payload, &out, headers); err != nil {
		return nil, fmt.Errorf("authorize continue: %w", err)
	}
	return out, nil
}

func (l *openAI401ProtocolLogin) verifyPassword(ctx context.Context, password string) (map[string]any, error) {
	headers := map[string]string{}
	if l.lastSentinel != "" {
		headers["openai-sentinel-token"] = l.lastSentinel
	}
	var out map[string]any
	if err := l.doJSON(ctx, http.MethodPost, l.authURL("/api/accounts/password/verify"), "https://auth.openai.com/log-in/password", map[string]string{"password": password}, &out, headers); err != nil {
		return nil, fmt.Errorf("password verify: %w", err)
	}
	return out, nil
}

func (l *openAI401ProtocolLogin) resendOTP(ctx context.Context) error {
	headers := map[string]string{}
	if l.lastSentinel != "" {
		headers["openai-sentinel-token"] = l.lastSentinel
	}
	var out map[string]any
	return l.doJSON(ctx, http.MethodPost, l.authURL("/api/accounts/email-otp/resend"), "https://auth.openai.com/email-verification", map[string]any{}, &out, headers)
}

func (l *openAI401ProtocolLogin) sendOTP(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, l.authURL("/api/accounts/email-otp/send"), nil)
	if err != nil {
		return err
	}
	applyOpenAI401Headers(req, "https://auth.openai.com/email-verification", "application/json")
	if l.lastSentinel != "" {
		req.Header.Set("openai-sentinel-token", l.lastSentinel)
	}
	resp, err := l.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("send OTP HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return nil
}

func (l *openAI401ProtocolLogin) verifyOTP(ctx context.Context, code string) (map[string]any, error) {
	var out map[string]any
	if err := l.doJSON(ctx, http.MethodPost, l.authURL("/api/accounts/email-otp/validate"), "https://auth.openai.com/email-verification", map[string]string{"code": code}, &out); err != nil {
		return nil, fmt.Errorf("verify OTP: %w", err)
	}
	return out, nil
}

func (l *openAI401ProtocolLogin) followRedirects(ctx context.Context, startURL string) (string, string, error) {
	current := startURL
	callbackURL := ""
	referer := "https://chatgpt.com/"
	for i := 0; i < 12 && current != ""; i++ {
		if strings.Contains(current, "/api/auth/callback/openai") && strings.Contains(current, "code=") {
			callbackURL = current
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, current, nil)
		if err != nil {
			return callbackURL, current, err
		}
		applyOpenAI401Headers(req, referer, "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
		resp, err := l.client.Do(req)
		if err != nil {
			return callbackURL, current, err
		}
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		if resp.StatusCode < 300 || resp.StatusCode > 399 {
			return callbackURL, current, nil
		}
		loc := resp.Header.Get("Location")
		if loc == "" {
			return callbackURL, current, nil
		}
		next, err := url.Parse(loc)
		if err != nil {
			return callbackURL, current, err
		}
		base, _ := url.Parse(current)
		referer = current
		current = base.ResolveReference(next).String()
		if strings.Contains(current, "/api/auth/callback/openai") && strings.Contains(current, "code=") {
			callbackURL = current
		}
	}
	return callbackURL, current, nil
}

func (l *openAI401ProtocolLogin) getAuthSession(ctx context.Context) (map[string]any, error) {
	var payload map[string]any
	if err := l.doJSON(ctx, http.MethodGet, l.chatGPTURL("/api/auth/session"), "https://chatgpt.com/", nil, &payload); err != nil {
		return nil, fmt.Errorf("get ChatGPT auth session: %w", err)
	}
	accessToken := firstReloginString(payload, "accessToken", "access_token")
	if accessToken == "" {
		return nil, errors.New("ChatGPT auth session missing accessToken")
	}
	sessionToken := l.cookieValue("chatgpt.com", "__Secure-next-auth.session-token")
	session := cloneCredentials(payload)
	session["accessToken"] = accessToken
	if sessionToken == "" {
		sessionToken = l.sessionToken
	}
	if sessionToken != "" {
		session["sessionToken"] = sessionToken
	}
	if l.email != "" {
		session["email"] = l.email
	}
	return session, nil
}

func (l *openAI401ProtocolLogin) captureAuthParams(rawURL string) (string, error) {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return "", err
	}
	q := parsed.Query()
	l.clientID = q.Get("client_id")
	l.redirectURI = q.Get("redirect_uri")
	l.state = q.Get("state")
	l.codeChallenge = q.Get("code_challenge")
	return rawURL, nil
}

func (l *openAI401ProtocolLogin) doJSON(ctx context.Context, method, target, referer string, input any, output *map[string]any, extraHeaders ...map[string]string) error {
	var body io.Reader
	if input != nil {
		raw, err := json.Marshal(input)
		if err != nil {
			return err
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return err
	}
	applyOpenAI401Headers(req, referer, "application/json")
	if l.sessionToken != "" && strings.Contains(target, "/api/auth/session") {
		req.AddCookie(&http.Cookie{Name: "__Secure-next-auth.session-token", Value: l.sessionToken})
	}
	if input != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for _, headers := range extraHeaders {
		for key, value := range headers {
			req.Header.Set(key, value)
		}
	}
	resp, err := l.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if resp.StatusCode >= 300 {
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncateRelogin401Error(string(raw)))
	}
	if output == nil {
		return nil
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(output); err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}

func (l *openAI401ProtocolLogin) doFormJSON(ctx context.Context, target, referer string, form url.Values, output *map[string]any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	applyOpenAI401Headers(req, referer, "application/json")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := l.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if resp.StatusCode >= 300 {
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncateRelogin401Error(string(raw)))
	}
	if output == nil {
		return nil
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	return decoder.Decode(output)
}

func (l *openAI401ProtocolLogin) cookieValue(domain, name string) string {
	if l == nil || l.client == nil || l.client.Jar == nil {
		return ""
	}
	u := &url.URL{Scheme: "https", Host: domain, Path: "/"}
	for _, cookie := range l.client.Jar.Cookies(u) {
		if cookie.Name == name {
			return cookie.Value
		}
	}
	return ""
}

func (l *openAI401ProtocolLogin) setCookie(domain, name, value string) {
	if l == nil || l.client == nil || l.client.Jar == nil {
		return
	}
	u := &url.URL{Scheme: "https", Host: domain, Path: "/"}
	l.client.Jar.SetCookies(u, []*http.Cookie{{Name: name, Value: value, Path: "/", Domain: "." + domain, Secure: true}})
}

func (l *openAI401ProtocolLogin) setCookieForBaseURL(baseURL, name, value string) {
	if l == nil || l.client == nil || l.client.Jar == nil || strings.TrimSpace(baseURL) == "" {
		return
	}
	parsed, err := url.Parse(baseURL)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return
	}
	l.client.Jar.SetCookies(parsed, []*http.Cookie{{Name: name, Value: value, Path: "/", Secure: parsed.Scheme == "https"}})
}

func applyOpenAI401Headers(req *http.Request, referer, accept string) {
	if accept == "" {
		accept = "application/json"
	}
	if strings.TrimSpace(referer) == "" {
		referer = "https://chatgpt.com/"
	}
	origin := "https://chatgpt.com"
	if parsed, err := url.Parse(referer); err == nil && parsed.Scheme != "" && parsed.Host != "" {
		origin = parsed.Scheme + "://" + parsed.Host
	}
	req.Header.Set("Accept", accept)
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	req.Header.Set("Origin", origin)
	req.Header.Set("Referer", referer)
	req.Header.Set("User-Agent", openAI401DefaultUserAgent)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	req.Header.Set("Sec-Fetch-Mode", "cors")
	req.Header.Set("Sec-Fetch-Dest", "empty")
	req.Header.Set("sec-ch-ua", `"Not:A-Brand";v="99", "Google Chrome";v="145", "Chromium";v="145"`)
	req.Header.Set("sec-ch-ua-mobile", "?0")
	req.Header.Set("sec-ch-ua-platform", `"Windows"`)
}

func extractOpenAI401PageType(resp map[string]any) string {
	if page, ok := mapValue(resp, "page"); ok {
		return firstReloginString(page, "type")
	}
	return ""
}

func extractOpenAI401ContinueURL(resp map[string]any) string {
	if value := firstReloginString(resp, "continue_url"); value != "" {
		return value
	}
	if page, ok := mapValue(resp, "page"); ok {
		if firstReloginString(page, "type") == "external_url" {
			return firstReloginString(page, "payload.url")
		}
	}
	return ""
}

func openAI401RequiresPhoneVerification(pageType, continueURL string) bool {
	value := strings.ToLower(strings.TrimSpace(pageType + " " + continueURL))
	if value == "" {
		return false
	}
	for _, marker := range []string{"phone", "add-phone", "phone-verification", "phone_number", "sms"} {
		if strings.Contains(value, marker) {
			return true
		}
	}
	return false
}

func openAI401RequiresPassword(pageType, continueURL string) bool {
	value := strings.ToLower(strings.TrimSpace(pageType + " " + continueURL))
	return strings.Contains(value, "login_password") || strings.Contains(value, "/log-in/password")
}

type openAI401Mailbox interface {
	WaitForOTP(ctx context.Context, email string, issuedAfter time.Time, timeout time.Duration) (string, error)
}

type cloudflareTempEmailMailbox struct {
	baseURL   string
	adminAuth string
	client    *http.Client
}

func newCloudflareTempEmailMailbox(baseURL, adminAuth string, client *http.Client) *cloudflareTempEmailMailbox {
	return &cloudflareTempEmailMailbox{
		baseURL:   strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		adminAuth: strings.TrimSpace(adminAuth),
		client:    client,
	}
}

func (m *cloudflareTempEmailMailbox) WaitForOTP(ctx context.Context, email string, issuedAfter time.Time, timeout time.Duration) (string, error) {
	if m == nil || m.baseURL == "" || m.adminAuth == "" {
		return "", errors.New("Cloudflare Temp Email config is incomplete")
	}
	deadline := time.Now().Add(timeout)
	var lastErr error
	for attempt := 1; time.Now().Before(deadline); attempt++ {
		messages, err := m.listMessages(ctx, email)
		if err != nil {
			lastErr = err
		} else {
			for _, msg := range messages {
				if !msg.receivedAt.IsZero() && msg.receivedAt.Before(issuedAfter) {
					continue
				}
				code := extractOpenAI401OTP(msg.subject + " " + msg.body + " " + msg.raw)
				if code == "" {
					if detail, err := m.messageDetail(ctx, msg); err == nil {
						msg = detail
						code = extractOpenAI401OTP(msg.subject + " " + msg.body + " " + msg.raw)
					}
				}
				if code != "" {
					if msg.id != "" {
						_ = m.deleteMessage(ctx, msg.id)
					}
					return code, nil
				}
			}
		}
		wait := 3 * time.Second
		if attempt > 10 {
			wait = 5 * time.Second
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return "", ctx.Err()
		case <-timer.C:
		}
	}
	if lastErr != nil {
		return "", fmt.Errorf("wait Cloudflare Temp Email OTP timeout: last error: %w", lastErr)
	}
	return "", errors.New("wait Cloudflare Temp Email OTP timeout")
}

func (m *cloudflareTempEmailMailbox) listMessages(ctx context.Context, email string) ([]openAI401MailMessage, error) {
	u, err := url.Parse(m.baseURL + "/admin/mails")
	if err != nil {
		return nil, err
	}
	q := u.Query()
	q.Set("limit", "50")
	if email != "" {
		q.Set("address", strings.ToLower(strings.TrimSpace(email)))
		q.Set("recipient", strings.ToLower(strings.TrimSpace(email)))
	}
	u.RawQuery = q.Encode()
	var payload any
	if err := m.requestJSON(ctx, http.MethodGet, u.String(), nil, &payload); err != nil {
		return nil, err
	}
	return normalizeOpenAI401MailMessages(payload), nil
}

func (m *cloudflareTempEmailMailbox) messageDetail(ctx context.Context, msg openAI401MailMessage) (openAI401MailMessage, error) {
	if msg.id == "" {
		return msg, nil
	}
	var payload any
	if err := m.requestJSON(ctx, http.MethodGet, m.baseURL+"/admin/mails/"+url.PathEscape(msg.id), nil, &payload); err != nil {
		return msg, err
	}
	messages := normalizeOpenAI401MailMessages(payload)
	if len(messages) == 0 {
		return msg, nil
	}
	if messages[0].id == "" {
		messages[0].id = msg.id
	}
	return messages[0], nil
}

func (m *cloudflareTempEmailMailbox) deleteMessage(ctx context.Context, id string) error {
	return m.requestJSON(ctx, http.MethodDelete, m.baseURL+"/admin/mails/"+url.PathEscape(id), nil, nil)
}

func (m *cloudflareTempEmailMailbox) requestJSON(ctx context.Context, method, target string, input any, output any) error {
	var body io.Reader
	if input != nil {
		raw, err := json.Marshal(input)
		if err != nil {
			return err
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("x-admin-auth", m.adminAuth)
	if input != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := m.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if resp.StatusCode >= 300 {
		return fmt.Errorf("Cloudflare Temp Email HTTP %d: %s", resp.StatusCode, truncateRelogin401Error(string(raw)))
	}
	if output == nil {
		return nil
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	return decoder.Decode(output)
}

type openAI401MailMessage struct {
	id         string
	address    string
	subject    string
	body       string
	raw        string
	receivedAt time.Time
}

func normalizeOpenAI401MailMessages(payload any) []openAI401MailMessage {
	rows := openAI401MailRows(payload)
	out := make([]openAI401MailMessage, 0, len(rows))
	for _, row := range rows {
		msg := normalizeOpenAI401MailMessage(row)
		if msg.subject != "" || msg.body != "" || msg.raw != "" {
			out = append(out, msg)
		}
	}
	return out
}

func openAI401MailRows(payload any) []map[string]any {
	switch v := payload.(type) {
	case []any:
		out := make([]map[string]any, 0, len(v))
		for _, item := range v {
			if row, ok := item.(map[string]any); ok {
				out = append(out, row)
			}
		}
		return out
	case map[string]any:
		for _, key := range []string{"data", "items", "messages", "mails", "results", "rows"} {
			if arr, ok := v[key].([]any); ok {
				return openAI401MailRows(arr)
			}
			if obj, ok := v[key].(map[string]any); ok {
				return []map[string]any{obj}
			}
		}
		return []map[string]any{v}
	default:
		return nil
	}
}

func normalizeOpenAI401MailMessage(row map[string]any) openAI401MailMessage {
	raw := firstReloginString(row, "raw", "source", "mime", "message")
	subject := decodeOpenAI401MIMEHeader(firstReloginString(row, "subject"))
	body := strings.Join([]string{
		openAI401ContentString(row["text"]),
		openAI401ContentString(row["text_content"]),
		openAI401ContentString(row["textContent"]),
		openAI401ContentString(row["plain"]),
		openAI401ContentString(row["body"]),
		openAI401ContentString(row["body_html"]),
		openAI401ContentString(row["bodyHtml"]),
		openAI401ContentString(row["content"]),
		openAI401ContentString(row["html"]),
		openAI401ContentString(row["html_content"]),
		openAI401ContentString(row["htmlContent"]),
		extractOpenAI401MIMEText(raw),
	}, " ")
	return openAI401MailMessage{
		id:         firstReloginString(row, "id", "mail_id"),
		address:    strings.ToLower(firstReloginString(row, "address", "mail_address", "email", "recipient")),
		subject:    subject,
		body:       normalizeOpenAI401Spaces(stripOpenAI401HTML(body)),
		raw:        raw,
		receivedAt: parseOpenAI401MailTime(firstReloginString(row, "receivedDateTime", "received_at", "created_at", "createdAt", "updated_at", "date")),
	}
}

func openAI401ContentString(value any) string {
	switch v := value.(type) {
	case string:
		return v
	case map[string]any:
		parts := make([]string, 0, len(v))
		for _, key := range []string{"text", "plain", "body", "content", "html"} {
			parts = append(parts, openAI401ContentString(v[key]))
		}
		return strings.Join(parts, " ")
	case []any:
		parts := make([]string, 0, len(v))
		for _, item := range v {
			parts = append(parts, openAI401ContentString(item))
		}
		return strings.Join(parts, " ")
	default:
		return ""
	}
}

func extractOpenAI401MIMEText(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	msg, err := mail.ReadMessage(strings.NewReader(raw))
	if err != nil {
		return raw
	}
	contentType := msg.Header.Get("Content-Type")
	mediaType, params, _ := mime.ParseMediaType(contentType)
	if strings.HasPrefix(mediaType, "multipart/") {
		mr := multipart.NewReader(msg.Body, params["boundary"])
		parts := []string{}
		for {
			part, err := mr.NextPart()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				break
			}
			rawPart, _ := io.ReadAll(io.LimitReader(part, 1<<20))
			parts = append(parts, stripOpenAI401HTML(string(rawPart)))
		}
		return strings.Join(parts, " ")
	}
	rawBody, _ := io.ReadAll(io.LimitReader(msg.Body, 1<<20))
	return stripOpenAI401HTML(string(rawBody))
}

func decodeOpenAI401MIMEHeader(value string) string {
	decoded, err := new(mime.WordDecoder).DecodeHeader(value)
	if err != nil {
		return value
	}
	return decoded
}

func stripOpenAI401HTML(value string) string {
	value = regexp.MustCompile(`(?is)<script.*?</script>|<style.*?</style>`).ReplaceAllString(value, " ")
	value = regexp.MustCompile(`(?is)<[^>]+>`).ReplaceAllString(value, " ")
	replacements := map[string]string{"&nbsp;": " ", "&amp;": "&", "&lt;": "<", "&gt;": ">"}
	for old, next := range replacements {
		value = strings.ReplaceAll(value, old, next)
	}
	return value
}

func normalizeOpenAI401Spaces(value string) string {
	return strings.Join(strings.Fields(value), " ")
}

func parseOpenAI401MailTime(value string) time.Time {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}
	}
	if n, err := strconv.ParseInt(value, 10, 64); err == nil {
		return reloginUnixTime(n)
	}
	if t, err := time.Parse(time.RFC3339Nano, value); err == nil {
		return t
	}
	if t, err := mail.ParseDate(value); err == nil {
		return t
	}
	return time.Time{}
}

var openAI401OTPPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)(?:code|verification|verify|otp)[^\d]{0,40}(\d{6})`),
	regexp.MustCompile(`\b(\d{6})\b`),
}

func extractOpenAI401OTP(text string) string {
	for _, pattern := range openAI401OTPPatterns {
		if match := pattern.FindStringSubmatch(text); len(match) > 1 {
			return match[1]
		}
	}
	return ""
}

func buildOpenAI401SentinelToken(ctx context.Context, client *http.Client, deviceID, flow string) (string, error) {
	if deviceID == "" {
		deviceID = newOpenAI401UUID()
	}
	generator := openAI401SentinelGenerator{deviceID: deviceID}
	reqPayload := map[string]any{
		"p":    generator.requirementsToken(),
		"id":   deviceID,
		"flow": flow,
	}
	raw, _ := json.Marshal(reqPayload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, openAI401SentinelReqURL, bytes.NewReader(raw))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "text/plain;charset=UTF-8")
	req.Header.Set("Accept", "*/*")
	req.Header.Set("Origin", "https://sentinel.openai.com")
	req.Header.Set("Referer", "https://sentinel.openai.com/backend-api/sentinel/frame.html")
	req.Header.Set("User-Agent", openAI401DefaultUserAgent)
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 300 {
		return "", fmt.Errorf("sentinel req HTTP %d: %s", resp.StatusCode, truncateRelogin401Error(string(body)))
	}
	var challenge map[string]any
	if err := json.Unmarshal(body, &challenge); err != nil {
		return "", err
	}
	cValue := openAI401String(challenge["token"])
	pValue := generator.requirementsToken()
	if pow, ok := challenge["proofofwork"].(map[string]any); ok && openAI401Bool(pow["required"]) {
		seed := openAI401String(pow["seed"])
		difficulty := firstNonEmptyOpenAI401(openAI401String(pow["difficulty"]), "0")
		if seed != "" {
			pValue = generator.powToken(seed, difficulty)
		}
	}
	tokenPayload := map[string]any{"p": pValue, "t": "", "c": cValue, "id": deviceID, "flow": flow}
	tokenRaw, _ := json.Marshal(tokenPayload)
	return string(tokenRaw), nil
}

type openAI401SentinelGenerator struct {
	deviceID string
}

func (g openAI401SentinelGenerator) requirementsToken() string {
	cfg := g.config()
	cfg[3] = float64(1)
	cfg[9] = float64(12)
	return "gAAAAAC" + base64.StdEncoding.EncodeToString(mustJSONOpenAI401(cfg))
}

func (g openAI401SentinelGenerator) powToken(seed, difficulty string) string {
	cfg := g.config()
	start := time.Now()
	for nonce := 0; nonce < 500000; nonce++ {
		cfg[3] = float64(nonce)
		cfg[9] = float64(time.Since(start).Milliseconds())
		encoded := base64.StdEncoding.EncodeToString(mustJSONOpenAI401(cfg))
		digest := fnv1aOpenAI401(seed + encoded)
		if strings.Compare(digest[:min(len(digest), len(difficulty))], difficulty) <= 0 {
			return "gAAAAAB" + encoded + "~S"
		}
	}
	return g.requirementsToken()
}

func (g openAI401SentinelGenerator) config() []any {
	now := time.Now().UTC().Format("Mon Jan 02 2006 15:04:05 GMT+0000 (Coordinated Universal Time)")
	return []any{
		"1920x1080", now, 4294705152, mathrand.Float64(), openAI401DefaultUserAgent,
		openAI401SentinelSDKURL, nil, nil, "en-US", "en-US,en", mathrand.Float64(),
		"vendor=Google Inc.", "location", "Object", mathrand.Float64() * 50000,
		newOpenAI401UUID(), "", 8, float64(time.Now().UnixMilli()),
	}
}

func mustJSONOpenAI401(value any) []byte {
	raw, _ := json.Marshal(value)
	return raw
}

func fnv1aOpenAI401(text string) string {
	h := uint32(2166136261)
	for _, ch := range text {
		h ^= uint32(ch)
		h *= 16777619
	}
	h ^= h >> 16
	h *= 2246822507
	h ^= h >> 13
	h *= 3266489909
	h ^= h >> 16
	return fmt.Sprintf("%08x", h)
}

func newOpenAI401UUID() string {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x", mathrand.Uint32(), mathrand.Uint32()&0xffff, mathrand.Uint32()&0xffff, mathrand.Uint32()&0xffff, mathrand.Uint64())
	}
	raw[6] = (raw[6] & 0x0f) | 0x40
	raw[8] = (raw[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", raw[0:4], raw[4:6], raw[6:8], raw[8:10], raw[10:])
}

func openAI401CredentialString(account *Account, keys ...string) string {
	if account == nil {
		return ""
	}
	for _, key := range keys {
		if value := strings.TrimSpace(account.GetCredential(key)); value != "" {
			return value
		}
	}
	return ""
}

func openAI401String(value any) string {
	return strings.TrimSpace(credentialValueAsString(value))
}

func openAI401Bool(value any) bool {
	switch v := value.(type) {
	case bool:
		return v
	case string:
		return strings.EqualFold(strings.TrimSpace(v), "true")
	default:
		return false
	}
}

func firstNonEmptyOpenAI401(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
