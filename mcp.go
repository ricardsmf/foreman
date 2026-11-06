package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"net"
	"net/http"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/modelcontextprotocol/go-sdk/oauthex"
	"github.com/zalando/go-keyring"
	"golang.org/x/oauth2"
)

// Atlassian's remote MCP, signed in with this app's own OAuth client (dynamic registration),
// kept in the macOS Keychain apart from pi's and Claude Code's.

const (
	serverURL       = "https://mcp.atlassian.com/v2/mcp"
	callbackPort    = 3127
	keychainService = "foreman"
	keychainAccount = "atlassian-mcp"
)

var redirectURL = fmt.Sprintf("http://localhost:%d/callback", callbackPort)

// The Keychain entry keeps the shape the TypeScript version wrote, so an existing sign-in carries over.
type storedClient struct {
	ClientID                string   `json:"client_id"`
	ClientSecret            string   `json:"client_secret,omitempty"`
	RedirectURIs            []string `json:"redirect_uris,omitempty"`
	TokenEndpointAuthMethod string   `json:"token_endpoint_auth_method,omitempty"`
	ClientName              string   `json:"client_name,omitempty"`
	Issuer                  string   `json:"issuer,omitempty"`
}

type storedTokens struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token,omitempty"`
	TokenType    string `json:"token_type,omitempty"`
	ExpiresIn    int64  `json:"expires_in,omitempty"`
	Scope        string `json:"scope,omitempty"`
	Issuer       string `json:"issuer,omitempty"`
	ExpiresAt    int64  `json:"expires_at,omitempty"`
}

type stored struct {
	Client   *storedClient `json:"client,omitempty"`
	Tokens   *storedTokens `json:"tokens,omitempty"`
	Verifier string        `json:"verifier,omitempty"`
}

func loadStored() stored {
	var s stored
	if raw, err := keyring.Get(keychainService, keychainAccount); err == nil {
		_ = json.Unmarshal([]byte(raw), &s)
	}
	return s
}

func saveStored(s stored) error {
	b, _ := json.Marshal(s)
	return keyring.Set(keychainService, keychainAccount, string(b))
}

func signOut() {
	_ = keyring.Delete(keychainService, keychainAccount)
}

// oauthHandler implements auth.OAuthHandler: it serves the stored token, refreshes it when the
// server rejects it, and falls back to the browser sign-in when the refresh fails.
type oauthHandler struct {
	mu  sync.Mutex
	s   stored
	log func(string)
}

func (h *oauthHandler) TokenSource(ctx context.Context) (oauth2.TokenSource, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.s.Tokens == nil {
		return nil, nil
	}
	return h, nil
}

func (h *oauthHandler) Token() (*oauth2.Token, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	t := h.s.Tokens
	if t == nil {
		return nil, errors.New("not signed in")
	}
	if t.ExpiresAt != 0 && time.Now().Add(time.Minute).Unix() >= t.ExpiresAt && t.RefreshToken != "" {
		if err := h.refresh(context.Background()); err != nil {
			return nil, &oauth2.RetrieveError{ErrorCode: "invalid_grant", ErrorDescription: err.Error()}
		}
		t = h.s.Tokens
	}
	return &oauth2.Token{AccessToken: t.AccessToken, TokenType: "Bearer"}, nil
}

func (h *oauthHandler) Authorize(ctx context.Context, req *http.Request, resp *http.Response) error {
	challenges, _ := oauthex.ParseWWWAuthenticate(resp.Header.Values("WWW-Authenticate"))
	_ = resp.Body.Close()
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.s.Tokens != nil && h.s.Tokens.RefreshToken != "" && h.refresh(ctx) == nil {
		return nil
	}
	return h.login(ctx, challenges)
}

func (h *oauthHandler) config(asm *oauthex.AuthServerMeta, scopes []string) *oauth2.Config {
	c := h.s.Client
	cfg := &oauth2.Config{
		ClientID:    c.ClientID,
		RedirectURL: redirectURL,
		Scopes:      scopes,
		Endpoint: oauth2.Endpoint{
			AuthURL:   asm.AuthorizationEndpoint,
			TokenURL:  asm.TokenEndpoint,
			AuthStyle: oauth2.AuthStyleInParams,
		},
	}
	if c.TokenEndpointAuthMethod != "none" {
		cfg.ClientSecret = c.ClientSecret
	}
	return cfg
}

func (h *oauthHandler) saveToken(tok *oauth2.Token, issuer string) {
	t := &storedTokens{
		AccessToken:  tok.AccessToken,
		RefreshToken: tok.RefreshToken,
		TokenType:    tok.TokenType,
		ExpiresIn:    tok.ExpiresIn,
		Issuer:       issuer,
	}
	if t.RefreshToken == "" && h.s.Tokens != nil {
		t.RefreshToken = h.s.Tokens.RefreshToken
	}
	if s, ok := tok.Extra("scope").(string); ok {
		t.Scope = s
	}
	if !tok.Expiry.IsZero() {
		t.ExpiresAt = tok.Expiry.Unix()
	}
	h.s.Tokens = t
	h.s.Verifier = ""
	_ = saveStored(h.s)
}

// refresh trades the stored refresh token for a new access token. Callers hold h.mu.
func (h *oauthHandler) refresh(ctx context.Context) error {
	t := h.s.Tokens
	if h.s.Client == nil || t.Issuer == "" {
		return errors.New("no client to refresh with")
	}
	asm, err := auth.GetAuthServerMetadata(ctx, t.Issuer, nil)
	if err != nil || asm == nil {
		return fmt.Errorf("authorization server metadata: %v", err)
	}
	expired := &oauth2.Token{RefreshToken: t.RefreshToken, Expiry: time.Unix(1, 0)}
	tok, err := h.config(asm, nil).TokenSource(ctx, expired).Token()
	if err != nil {
		return err
	}
	h.saveToken(tok, t.Issuer)
	return nil
}

// login runs the browser authorization-code flow with PKCE. Callers hold h.mu.
func (h *oauthHandler) login(ctx context.Context, challenges []oauthex.Challenge) error {
	metaURL := ""
	for _, c := range challenges {
		if u := c.Params["resource_metadata"]; u != "" {
			metaURL = u
		}
	}
	if metaURL == "" {
		metaURL = "https://mcp.atlassian.com/.well-known/oauth-protected-resource/v2/mcp"
	}
	prm, err := oauthex.GetProtectedResourceMetadata(ctx, metaURL, serverURL, nil)
	if err != nil {
		return fmt.Errorf("protected resource metadata: %w", err)
	}
	if len(prm.AuthorizationServers) == 0 {
		return errors.New("the MCP server names no authorization server")
	}
	issuer := prm.AuthorizationServers[0]
	asm, err := auth.GetAuthServerMetadata(ctx, issuer, nil)
	if err != nil || asm == nil {
		return fmt.Errorf("authorization server metadata: %v", err)
	}

	if h.s.Client == nil || h.s.Client.Issuer != issuer {
		reg, err := oauthex.RegisterClient(ctx, asm.RegistrationEndpoint, &oauthex.ClientRegistrationMetadata{
			ClientName:              "foreman",
			RedirectURIs:            []string{redirectURL},
			GrantTypes:              []string{"authorization_code", "refresh_token"},
			ResponseTypes:           []string{"code"},
			TokenEndpointAuthMethod: "none",
		}, nil)
		if err != nil {
			return fmt.Errorf("client registration: %w", err)
		}
		h.s.Client = &storedClient{
			ClientID:                reg.ClientID,
			ClientSecret:            reg.ClientSecret,
			RedirectURIs:            []string{redirectURL},
			TokenEndpointAuthMethod: "none",
			ClientName:              "foreman",
			Issuer:                  issuer,
		}
		_ = saveStored(h.s)
	}

	cfg := h.config(asm, prm.ScopesSupported)
	verifier := oauth2.GenerateVerifier()
	h.s.Verifier = verifier
	_ = saveStored(h.s)
	state := randomHex(16)
	authURL := cfg.AuthCodeURL(state, oauth2.S256ChallengeOption(verifier), oauth2.SetAuthURLParam("resource", serverURL))

	// The sign-in gets its own five minutes, not what is left of the Jira call that triggered it.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Minute)
	defer cancel()
	codes, err := waitForCode(ctx, state)
	if err != nil {
		return err
	}
	h.log("Opening the browser to sign in to Atlassian…\n" + authURL)
	_ = exec.Command("open", authURL).Start()
	var code string
	select {
	case r := <-codes:
		if r.err != nil {
			return r.err
		}
		code = r.code
	case <-ctx.Done():
		return errors.New("atlassian sign-in timed out")
	}
	tok, err := cfg.Exchange(ctx, code, oauth2.VerifierOption(verifier), oauth2.SetAuthURLParam("resource", serverURL))
	if err != nil {
		return fmt.Errorf("token exchange: %w", err)
	}
	h.saveToken(tok, issuer)
	return nil
}

type codeResult struct {
	code string
	err  error
}

func waitForCode(ctx context.Context, state string) (<-chan codeResult, error) {
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", callbackPort))
	if err != nil {
		return nil, err
	}
	out := make(chan codeResult, 1)
	srv := &http.Server{}
	var once sync.Once
	finish := func(r codeResult) {
		once.Do(func() {
			out <- r
			go func() { _ = srv.Close() }()
		})
	}
	srv.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/callback" {
			http.NotFound(w, r)
			return
		}
		q := r.URL.Query()
		// Only the authorization server's redirect carries our state; any other request, such as a
		// link opened elsewhere, mustn't end the sign-in.
		if q.Get("state") != state {
			http.Error(w, "state mismatch", http.StatusBadRequest)
			return
		}
		code := q.Get("code")
		msg := cmp(q.Get("error_description"), q.Get("error"), "no code returned")
		w.Header().Set("content-type", "text/html; charset=utf-8")
		page := "foreman is signed in. You can close this tab."
		if code == "" {
			page = "Sign-in failed: " + html.EscapeString(msg)
		}
		_, _ = fmt.Fprintf(w, `<p style="font-family:system-ui">%s</p>`, page)
		if code != "" {
			finish(codeResult{code: code})
		} else {
			finish(codeResult{err: fmt.Errorf("atlassian sign-in failed: %s", msg)})
		}
	})
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			finish(codeResult{err: fmt.Errorf("sign-in callback server: %w", err)})
		}
	}()
	go func() {
		<-ctx.Done()
		_ = srv.Close()
	}()
	return out, nil
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

type Mcp struct {
	session *mcp.ClientSession
}

// connect opens the MCP session, running the browser sign-in first when there are no valid tokens.
func connect(ctx context.Context, log func(string)) (*Mcp, error) {
	h := &oauthHandler{s: loadStored(), log: log}
	client := mcp.NewClient(&mcp.Implementation{Name: "foreman", Version: "0.1.0"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: serverURL, OAuthHandler: h}, nil)
	if err != nil {
		return nil, err
	}
	return &Mcp{session: session}, nil
}

// call runs a tool and decodes its JSON text result, unwrapped from a top-level `data` if present.
func (m *Mcp) call(ctx context.Context, tool string, args map[string]any, out any) error {
	res, err := m.session.CallTool(ctx, &mcp.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		return err
	}
	var sb strings.Builder
	for _, c := range res.Content {
		if t, ok := c.(*mcp.TextContent); ok {
			sb.WriteString(t.Text)
		}
	}
	text := sb.String()
	if res.IsError {
		if len(text) > 500 {
			text = text[:500]
		}
		return fmt.Errorf("%s: %s", tool, text)
	}
	var wrapper struct {
		Data json.RawMessage `json:"data"`
	}
	if json.Unmarshal([]byte(text), &wrapper) == nil && len(wrapper.Data) > 0 && string(wrapper.Data) != "null" {
		return json.Unmarshal(wrapper.Data, out)
	}
	return json.Unmarshal([]byte(text), out)
}

func (m *Mcp) close() error { return m.session.Close() }
