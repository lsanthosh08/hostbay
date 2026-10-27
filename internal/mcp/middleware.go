package mcp

import (
	"context"
	"net/http"
	"strconv"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/auth"
)

// Caps on what one request or one token may hold (KTD35).
const (
	maxBody        = 1 << 20
	maxInFlight    = 16 // requests one token may have open at once
	principalScope = "mcp:"
)

// guard lets a request through only when it names this listener exactly
// as its Host and carries no Origin header at all, so a page a browser
// loaded, even one reached by DNS rebinding, gets 403 (KTD35). Only MCP
// clients, which send no Origin, are supported.
func guard(port int, next http.Handler) http.Handler {
	hosts := map[string]bool{"127.0.0.1:" + strconv.Itoa(port): true, "localhost:" + strconv.Itoa(port): true}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := r.Header["Origin"]; ok || !hosts[r.Host] {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// bearer checks the token against the stored hashes; a missing or unknown
// one gets 401. The token's ID becomes the request's principal.
func bearer(tokens *Tokens, next http.Handler) http.Handler {
	verify := func(_ context.Context, secret string, _ *http.Request) (*auth.TokenInfo, error) {
		tok, ok := tokens.Verify(secret)
		if !ok {
			return nil, auth.ErrInvalidToken
		}
		return &auth.TokenInfo{UserID: tok.ID, Extra: map[string]any{"name": tok.Name}}, nil
	}
	return auth.RequireBearerToken(verify, &auth.RequireBearerTokenOptions{AllowMissingExpiration: true})(capInFlight(next))
}

// capInFlight refuses a token's request beyond maxInFlight open ones.
func capInFlight(next http.Handler) http.Handler {
	var mu sync.Mutex
	open := map[string]int{}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := auth.TokenInfoFromContext(r.Context()).UserID
		mu.Lock()
		if open[id] >= maxInFlight {
			mu.Unlock()
			http.Error(w, "too many requests from this token at once", http.StatusTooManyRequests)
			return
		}
		open[id]++
		mu.Unlock()
		defer func() {
			mu.Lock()
			if open[id]--; open[id] == 0 {
				delete(open, id)
			}
			mu.Unlock()
		}()
		next.ServeHTTP(w, r)
	})
}

// requestKey carries the HTTP request's own context to the tool handler.
// The SDK runs handlers on a context it detaches from the request, so a
// client that hangs up would otherwise leave its card waiting.
type requestKey struct{}

// keepRequest records r's context for clientGone.
func keepRequest(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), requestKey{}, r.Context())))
	})
}

// clientGone is a context that ends when the tool call's HTTP request
// does, or ctx when there is none.
func clientGone(ctx context.Context) context.Context {
	if rc, ok := ctx.Value(requestKey{}).(context.Context); ok {
		return rc
	}
	return ctx
}

// caller is the request's principal and the client's name.
func caller(ti *auth.TokenInfo) (principal, client string) {
	if ti == nil {
		return "", ""
	}
	name, _ := ti.Extra["name"].(string)
	return principalScope + ti.UserID, name
}
