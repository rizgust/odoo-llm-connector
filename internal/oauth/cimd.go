package oauth

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"time"
)

// Hosts whose client ID metadata documents we fetch (Claude, Claude Code, ChatGPT). Limiting
// this keeps the server from being used to fetch arbitrary URLs.
var metadataDocHosts = []string{"claude.ai", "claude.com", "chatgpt.com", "chat.openai.com", "openai.com"}

const docCacheTTL = time.Hour

type clientDoc struct {
	redirects []string
	at        time.Time
}

// documentRedirects returns the redirect URIs declared by a client ID metadata document
// (the client ID itself is an https URL pointing at the document).
func (s *Server) documentRedirects(ctx context.Context, clientID string) ([]string, bool) {
	u, err := url.Parse(clientID)
	if err != nil || u.Scheme != "https" || u.Path == "" || !slices.Contains(metadataDocHosts, u.Hostname()) {
		return nil, false
	}
	s.mu.Lock()
	cached, ok := s.docs[clientID]
	s.mu.Unlock()
	if ok && s.now().Sub(cached.at) < docCacheTTL {
		return cached.redirects, true
	}

	body, err := s.fetchDoc(ctx, clientID)
	if err != nil {
		return nil, false
	}
	var doc struct {
		ClientID     string   `json:"client_id"`
		RedirectURIs []string `json:"redirect_uris"`
	}
	if json.Unmarshal(body, &doc) != nil || doc.ClientID != clientID || len(doc.RedirectURIs) == 0 {
		return nil, false
	}
	s.mu.Lock()
	s.docs[clientID] = clientDoc{doc.RedirectURIs, s.now()}
	s.mu.Unlock()
	return doc.RedirectURIs, true
}

func fetchDocument(ctx context.Context, docURL string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, docURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("client metadata document: HTTP %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 64<<10))
}

// redirectMatches compares a requested redirect URI with a registered one. Loopback
// redirects match on any port (RFC 8252 §7.3): native apps like Claude Code pick a free
// port at sign-in time.
func redirectMatches(registered []string, requested string) bool {
	if slices.Contains(registered, requested) {
		return true
	}
	req, err := url.Parse(requested)
	if err != nil || req.Scheme != "http" || !isLoopback(req.Hostname()) {
		return false
	}
	for _, r := range registered {
		reg, err := url.Parse(r)
		if err == nil && reg.Scheme == "http" && reg.Hostname() == req.Hostname() && reg.Path == req.Path && reg.RawQuery == req.RawQuery {
			return true
		}
	}
	return false
}

func isLoopback(host string) bool {
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}
