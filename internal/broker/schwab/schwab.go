// Package schwab adapts the read-only Schwab Trader API market data
// endpoints (option chains and quotes) to broker.Broker.
//
// Endpoint and field names follow the official API specification:
// https://developer.schwab.com/ (Trader API → Market Data API). If Schwab
// changes the spec, update here. Credentials come from the environment
// via cmd/boxpilot and are never logged.
//
// Read-only red line: this package only performs GETs for market data
// plus the OAuth token refresh required to authenticate them.
package schwab

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"boxpilot/internal/box"
	"boxpilot/internal/broker"
	"boxpilot/internal/logx"
)

// DefaultBaseURL is the Schwab API host.
const DefaultBaseURL = "https://api.schwabapi.com"

// TokenStoreEnv names the environment variable that overrides where the
// rotated OAuth refresh token is persisted.
const TokenStoreEnv = "SCHWAB_TOKEN_STORE"

// refreshTokenEnv is the environment variable holding the refresh token
// that Schwab rotates. It is only referenced in log messages.
const refreshTokenEnv = "SCHWAB_REFRESH_TOKEN"

// Retry policy for read-only market data requests (§4.4).
const (
	maxAttempts = 3
	baseBackoff = 500 * time.Millisecond
)

// Config wires the client.
type Config struct {
	ClientID     string
	ClientSecret string
	RefreshToken string
	BaseURL      string // default DefaultBaseURL; override in tests
	HTTPClient   *http.Client
	Logger       *logx.Logger // optional; secrets are masked by the caller
	// TokenStorePath is where a rotated OAuth refresh token is
	// persisted. Schwab invalidates the previous refresh token on every
	// refresh, so without persistence a restart would leave the tool
	// holding a dead credential. Empty disables persistence and keeps
	// this package free of filesystem side effects — the CLI passes
	// DefaultTokenStorePath().
	TokenStorePath string
	// Sleep is the backoff sleeper; override in tests. Defaults to
	// time.Sleep.
	Sleep func(time.Duration)
}

// Client is a read-only Schwab market data client.
type Client struct {
	cfg   Config
	base  string
	http  *http.Client
	log   *logx.Logger
	sleep func(time.Duration)

	storePath string
	// altRefreshToken is the token that was replaced by a rotation. It
	// is kept only as a last-resort fallback in case the persisted
	// token was revoked.
	altRefreshToken string

	mu          sync.Mutex
	accessToken string
	expiresAt   time.Time
}

// DefaultTokenStorePath returns where the rotated refresh token is
// persisted: $SCHWAB_TOKEN_STORE when set, otherwise
// ~/.config/boxpilot/schwab_refresh_token. It returns "" when no
// location can be resolved, which disables persistence.
func DefaultTokenStorePath() string {
	if p := os.Getenv(TokenStoreEnv); p != "" {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".config", "boxpilot", "schwab_refresh_token")
}

// loadStoredToken reads the persisted refresh token. A missing or
// unreadable file simply yields "".
func loadStoredToken(path string) string {
	if path == "" {
		return ""
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(raw))
}

// saveToken persists a refresh token with owner-only permissions. The
// value is written to a temporary file first and renamed into place, so
// an interrupted write can never leave a truncated credential behind.
func saveToken(path, token string) error {
	if path == "" {
		return errors.New("no token store path")
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".schwab_token_*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.WriteString(token + "\n"); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

// New validates the config and returns a client.
func New(cfg Config) (*Client, error) {
	base := cfg.BaseURL
	if base == "" {
		base = DefaultBaseURL
	}
	hc := cfg.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}

	// Schwab rotates the refresh token on every refresh and
	// invalidates the old one. A token persisted by an earlier run is
	// therefore newer than the environment variable and wins; the
	// environment value is kept as a fallback for the case where the
	// persisted token was revoked.
	refresh := cfg.RefreshToken
	alt := ""
	if cfg.TokenStorePath != "" {
		if stored := loadStoredToken(cfg.TokenStorePath); stored != "" {
			switch {
			case refresh == "":
				refresh = stored
			case stored != refresh:
				alt, refresh = refresh, stored
			}
		}
	}
	if cfg.ClientID == "" || cfg.ClientSecret == "" || refresh == "" {
		return nil, errors.New("schwab: client id, app secret and refresh token are required")
	}
	cfg.RefreshToken = refresh

	lg := cfg.Logger
	if lg == nil {
		lg = logx.New(io.Discard, "schwab ", nil)
	}
	lg.SetSecrets([]string{cfg.ClientSecret, refresh, alt})

	sleep := cfg.Sleep
	if sleep == nil {
		sleep = time.Sleep
	}
	return &Client{
		cfg:             cfg,
		base:            strings.TrimRight(base, "/"),
		http:            hc,
		log:             lg,
		sleep:           sleep,
		storePath:       cfg.TokenStorePath,
		altRefreshToken: alt,
	}, nil
}

// compile-time interface check
var _ broker.Broker = (*Client)(nil)

// tokenResponse is the OAuth refresh response. Schwab rotates refresh
// tokens on every refresh; the new value replaces the old one in memory.
type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int64  `json:"expires_in"`
	TokenType    string `json:"token_type"`
}

// token returns a valid access token, refreshing it when expired or
// close to expiry (60s skew).
//
// The refresh token used for the exchange is the one persisted in the
// token store (Schwab rotates it on every refresh and invalidates the
// old one). The token from the environment is kept as a fallback for
// the case where the persisted token was revoked.
func (c *Client) token(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.accessToken != "" && time.Now().Before(c.expiresAt.Add(-60*time.Second)) {
		return c.accessToken, nil
	}

	used := c.cfg.RefreshToken
	access, rotated, err := c.refresh(ctx, used)
	if err != nil && c.altRefreshToken != "" && c.altRefreshToken != used {
		c.log.Printf("schwab token refresh failed with the persisted token; retrying with the configured token")
		used = c.altRefreshToken
		access, rotated, err = c.refresh(ctx, used)
	}
	if err != nil {
		return "", err
	}
	if rotated != "" {
		// Rotation: persist the new value so a restart keeps working.
		// The replaced token is retained only as a fallback.
		c.cfg.RefreshToken = rotated
		c.altRefreshToken = used
		c.persistRefreshToken(rotated)
	} else {
		// No rotation: the token that worked becomes the primary one
		// (it may have been the fallback after a retry).
		c.cfg.RefreshToken = used
	}
	c.log.SetSecrets([]string{c.cfg.ClientSecret, c.cfg.RefreshToken, c.altRefreshToken, c.accessToken})
	return access, nil
}

// refresh performs the OAuth refresh-token exchange. rotated is the new
// refresh token when Schwab rotated it, and "" otherwise.
func (c *Client) refresh(ctx context.Context, refreshToken string) (access string, rotated string, err error) {
	form := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshToken},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/v1/oauth/token", strings.NewReader(form.Encode()))
	if err != nil {
		return "", "", err
	}
	basic := base64.StdEncoding.EncodeToString([]byte(c.cfg.ClientID + ":" + c.cfg.ClientSecret))
	req.Header.Set("Authorization", "Basic "+basic)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("oauth token request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return "", "", fmt.Errorf("oauth token request: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var tr tokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&tr); err != nil {
		return "", "", fmt.Errorf("oauth token response: %w", err)
	}
	if tr.AccessToken == "" {
		return "", "", errors.New("oauth token response: empty access_token")
	}
	c.accessToken = tr.AccessToken
	c.expiresAt = time.Now().Add(time.Duration(tr.ExpiresIn) * time.Second)
	if tr.RefreshToken != "" && tr.RefreshToken != refreshToken {
		rotated = tr.RefreshToken
	}
	return c.accessToken, rotated, nil
}

// persistRefreshToken writes a rotated refresh token to the token store
// so that it survives a restart. A failure is reported but never fatal:
// the in-memory token keeps the current process working.
func (c *Client) persistRefreshToken(tok string) {
	if c.storePath == "" {
		c.log.Printf("schwab refresh token rotated but no token store is configured; update %s before restarting", refreshTokenEnv)
		return
	}
	if err := saveToken(c.storePath, tok); err != nil {
		c.log.Printf("schwab refresh token rotated but could not be persisted to %s: %v; update %s manually before restarting", c.storePath, err, refreshTokenEnv)
		return
	}
	c.log.Printf("schwab refresh token rotated; persisted to %s", c.storePath)
}

// apiError is a non-2xx market data response.
type apiError struct {
	Status int
	Body   string
}

func (e *apiError) Error() string { return fmt.Sprintf("schwab API: HTTP %d: %s", e.Status, e.Body) }

// getJSON performs a GET with the bearer token and the read-only retry
// policy (§4.4): up to 3 attempts, exponential backoff; HTTP 429 backs
// off using Retry-After when present.
func (c *Client) getJSON(ctx context.Context, path string, query url.Values, out any) error {
	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if attempt > 0 {
			backoff := baseBackoff << (attempt - 1)
			c.sleep(backoff)
		}
		tok, err := c.token(ctx)
		if err != nil {
			return err // auth failures are not retried here
		}
		u := c.base + path
		if len(query) > 0 {
			u += "?" + query.Encode()
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+tok)
		req.Header.Set("Accept", "application/json")

		resp, err := c.http.Do(req)
		if err != nil {
			lastErr = err
			c.log.Printf("schwab GET %s: %v (attempt %d/%d)", path, err, attempt+1, maxAttempts)
			continue
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		resp.Body.Close()
		if err != nil {
			lastErr = err
			continue
		}
		switch {
		case resp.StatusCode == http.StatusTooManyRequests:
			wait := backoffForRetryAfter(resp.Header.Get("Retry-After"), baseBackoff*(1<<attempt))
			c.log.Printf("schwab GET %s: rate limited (429); backing off %s", path, wait)
			c.sleep(wait)
			lastErr = &apiError{Status: resp.StatusCode, Body: string(body)}
			continue
		case resp.StatusCode >= 500:
			lastErr = &apiError{Status: resp.StatusCode, Body: string(body)}
			continue
		case resp.StatusCode != http.StatusOK:
			return &apiError{Status: resp.StatusCode, Body: string(body)}
		}
		if err := json.Unmarshal(body, out); err != nil {
			return fmt.Errorf("decode %s: %w", path, err)
		}
		return nil
	}
	return fmt.Errorf("schwab GET %s: gave up after %d attempts: %w", path, maxAttempts, lastErr)
}

// backoffForRetryAfter parses a Retry-After header (seconds) with a
// fallback duration.
func backoffForRetryAfter(h string, fallback time.Duration) time.Duration {
	if h == "" {
		return fallback
	}
	if secs, err := strconv.Atoi(strings.TrimSpace(h)); err == nil && secs >= 0 {
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(h); err == nil {
		return time.Until(t)
	}
	return fallback
}

// f2p converts a JSON float price in points to Price (4dp, rounded).
func f2p(f float64) box.Price { return box.Price(math.Round(f * box.PriceScale)) }

// msTime converts epoch milliseconds to time.Time.
func msTime(ms int64) time.Time { return time.UnixMilli(ms).UTC() }

// chainContract is one entry inside callExpDateMap/putExpDateMap. Field
// names follow the Schwab Market Data API spec.
type chainContract struct {
	Symbol       string  `json:"symbol"`
	Bid          float64 `json:"bidPrice"`
	Ask          float64 `json:"askPrice"`
	Last         float64 `json:"lastPrice"`
	Strike       float64 `json:"strikePrice"`
	Volume       float64 `json:"totalVolume"`
	OpenInterest float64 `json:"openInterest"`
	ExpirationMS int64   `json:"expirationDate"`
	QuoteTimeMS  int64   `json:"quoteTime"`
}

// chainResponse is the /marketdata/v1/chains payload (subset).
type chainResponse struct {
	Symbol         string                                `json:"symbol"`
	Status         string                                `json:"status"`
	Delayed        bool                                  `json:"isDelayed"`
	Index          bool                                  `json:"isIndex"`
	CallExpDateMap map[string]map[string][]chainContract `json:"callExpDateMap"`
	PutExpDateMap  map[string]map[string][]chainContract `json:"putExpDateMap"`
}

// GetOptionChain fetches one expiry's full chain in a single request
// (§3.2-1): contractType=ALL pinned to [expiry, expiry].
func (c *Client) GetOptionChain(ctx context.Context, symbol string, expiry time.Time) (broker.OptionChain, error) {
	date := expiry.Format("2006-01-02")
	q := url.Values{
		"symbol":                 {symbol},
		"contractType":           {"ALL"},
		"includeUnderlyingQuote": {"true"},
		"strategy":               {"SINGLE"},
		"fromDate":               {date},
		"toDate":                 {date},
	}
	var cr chainResponse
	if err := c.getJSON(ctx, "/marketdata/v1/chains", q, &cr); err != nil {
		return broker.OptionChain{}, err
	}
	if cr.Status != "" && !strings.EqualFold(cr.Status, "SUCCESS") {
		return broker.OptionChain{}, fmt.Errorf("schwab chains: status %q", cr.Status)
	}
	if cr.Delayed {
		c.log.Printf("schwab chains for %s %s are delayed quotes", symbol, date)
	}

	// Index contracts by strike, keyed off the date portion of
	// "YYYY-MM-DD:DTE" map keys.
	byStrike := map[int64]*broker.StrikeQuotes{}
	var idx []int64
	var latestQuote int64
	add := func(m map[string]map[string][]chainContract, put bool) {
		for expKey, strikes := range m {
			if strings.SplitN(expKey, ":", 2)[0] != date {
				continue
			}
			for _, contracts := range strikes {
				if len(contracts) == 0 {
					continue
				}
				cc := contracts[0]
				strike := int64(math.Round(cc.Strike))
				sq, ok := byStrike[strike]
				if !ok {
					sq = &broker.StrikeQuotes{Strike: strike}
					byStrike[strike] = sq
					idx = append(idx, strike)
				}
				ctr := &broker.Contract{
					Bid: f2p(cc.Bid), Ask: f2p(cc.Ask), Last: f2p(cc.Last),
					Volume: int64(cc.Volume), Interest: int64(cc.OpenInterest),
				}
				if put {
					sq.Put = ctr
				} else {
					sq.Call = ctr
				}
				if cc.QuoteTimeMS > latestQuote {
					latestQuote = cc.QuoteTimeMS
				}
			}
		}
	}
	add(cr.CallExpDateMap, false)
	add(cr.PutExpDateMap, true)
	sort.Slice(idx, func(i, j int) bool { return idx[i] < idx[j] })

	out := broker.OptionChain{
		Symbol:  symbol,
		Expiry:  expiry,
		Strikes: make([]broker.StrikeQuotes, 0, len(idx)),
	}
	for _, s := range idx {
		out.Strikes = append(out.Strikes, *byStrike[s])
	}
	if latestQuote > 0 {
		out.Timestamp = msTime(latestQuote)
	} else {
		out.Timestamp = time.Now().UTC()
	}
	return out, nil
}

// quoteEntry is one symbol's entry in the /quotes response.
type quoteEntry struct {
	Quote struct {
		LastPrice float64 `json:"lastPrice"`
		QuoteTime int64   `json:"quoteTime"`
	} `json:"quote"`
}

// GetQuote fetches the underlying quote. Index symbols are requested as
// "$SPX" (Schwab convention) with a fallback to the plain symbol.
func (c *Client) GetQuote(ctx context.Context, symbol string) (broker.Quote, error) {
	candidates := []string{"$" + symbol, symbol}
	var lastErr error
	for _, sym := range candidates {
		q := url.Values{"symbols": {sym}, "fields": {"quote"}}
		var qr map[string]quoteEntry
		if err := c.getJSON(ctx, "/marketdata/v1/quotes", q, &qr); err != nil {
			lastErr = err
			continue
		}
		entry, ok := qr[sym]
		if !ok || entry.Quote.QuoteTime == 0 {
			continue
		}
		return broker.Quote{
			Price:     f2p(entry.Quote.LastPrice),
			Timestamp: msTime(entry.Quote.QuoteTime),
		}, nil
	}
	if lastErr != nil {
		return broker.Quote{}, lastErr
	}
	return broker.Quote{}, fmt.Errorf("%w: quote for %s", broker.ErrNotFound, symbol)
}
