package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"

	"github.com/christianselig/apollo-backend/internal/domain"
	"github.com/christianselig/apollo-backend/internal/reddit"
)

// liveActivityRequest is the payload Apollo posts when it starts a Live
// Activity for a thread. AccessToken proves that the caller controls the
// requested Reddit account. The worker still polls Reddit through the
// registered account row, which remains the source of truth for refresh tokens
// and per-account Reddit client credentials.
type liveActivityRequest struct {
	APNSToken       string `json:"apns_token"`
	RedditAccountID string `json:"reddit_account_id"`
	AccessToken     string `json:"access_token"`
	RefreshToken    string `json:"refresh_token"`
	ThreadID        string `json:"thread_id"`
	Subreddit       string `json:"subreddit"`
	Development     bool   `json:"development"`
	SandboxReceipt  string `json:"sandboxReceipt"`
}

// UnmarshalJSON accepts both snake_case and the camelCase token keys Apollo's
// iOS client emits elsewhere, mirroring accountRegistrationRequest.
func (r *liveActivityRequest) UnmarshalJSON(data []byte) error {
	type alias liveActivityRequest
	aux := struct {
		AccessTokenCamel  string `json:"accessToken"`
		RefreshTokenCamel string `json:"refreshToken"`
		*alias
	}{alias: (*alias)(r)}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	if r.AccessToken == "" {
		r.AccessToken = aux.AccessTokenCamel
	}
	if r.RefreshToken == "" {
		r.RefreshToken = aux.RefreshTokenCamel
	}
	return nil
}

func (a *api) createLiveActivityHandler(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	req := &liveActivityRequest{}
	if err := json.NewDecoder(r.Body).Decode(req); err != nil {
		a.errorResponse(w, r, 400, err)
		return
	}

	// Live Activity pushes go straight to APNs (no Bark path exists), so a
	// Bark-only backend rejects the registration up front — this is what
	// keeps live_activities rows from piling up in that mode.
	if a.apns == nil {
		a.errorResponse(w, r, 422, fmt.Errorf("Live Activities require APNs, which this backend has not configured (Bark-only mode)"))
		return
	}

	// Apollo stores accounts by bare Reddit ID (me.ID); tolerate a t2_
	// fullname in case the client sends one.
	rid := strings.TrimPrefix(req.RedditAccountID, "t2_")

	// Same gateway-selection logic as device registration: the client's flag
	// is a hint, APPLE_APNS_SANDBOX pins it to match the build's signing.
	dev := req.Development || req.SandboxReceipt != ""
	if v := os.Getenv("APPLE_APNS_SANDBOX"); v != "" {
		dev = v == "1" || strings.EqualFold(v, "true")
	}

	la := &domain.LiveActivity{
		APNSToken:       req.APNSToken,
		Development:     dev,
		RedditAccountID: rid,
		ThreadID:        req.ThreadID,
		Subreddit:       req.Subreddit,
	}

	if err := la.Validate(); err != nil {
		a.errorResponse(w, r, 422, err)
		return
	}

	accessToken := strings.TrimSpace(req.AccessToken)
	if accessToken == "" {
		a.errorResponse(w, r, http.StatusUnauthorized, fmt.Errorf("Reddit authorization is required"))
		return
	}
	if a.redditIdentity == nil {
		a.errorResponse(w, r, http.StatusServiceUnavailable, fmt.Errorf("Reddit identity verification is unavailable"))
		return
	}

	identity, err := a.redditIdentity.MeWithAccessToken(ctx, accessToken)
	if err != nil {
		if errors.Is(err, reddit.ErrOauthRevoked) {
			a.errorResponse(w, r, http.StatusUnauthorized, fmt.Errorf("Reddit authorization is invalid"))
		} else {
			a.errorResponse(w, r, http.StatusBadGateway, fmt.Errorf("Reddit identity verification failed"))
		}
		return
	}
	if identity == nil || strings.TrimPrefix(identity.ID, "t2_") != rid {
		a.errorResponse(w, r, http.StatusUnauthorized, fmt.Errorf("Reddit authorization does not match the requested account"))
		return
	}

	if _, err := a.accountRepo.GetByRedditID(ctx, rid); err != nil {
		a.errorResponse(w, r, http.StatusUnprocessableEntity, fmt.Errorf("account not registered with this backend; open Apollo with notifications configured first"))
		return
	}

	if err := a.liveActivityRepo.Create(ctx, la); err != nil {
		a.errorResponse(w, r, 500, err)
		return
	}

	w.WriteHeader(http.StatusOK)
}
