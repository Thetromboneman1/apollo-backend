package api

import (
	"errors"
	"net/http"

	"github.com/gorilla/mux"
)

var (
	errCompatAccountNotFound  = errors.New("registered account not found")
	errCompatAccountAmbiguous = errors.New("multiple accounts are registered for this device")
	errCompatAccountLookup    = errors.New("failed to resolve registered account")
	errCompatAccountInvalid   = errors.New("registered account has no account identifier")
)

// resolveEmptyAccountCompatibility supports Apollo archives whose account
// object lost its Reddit fullname and consequently emits an empty account path
// segment. The device token is the only identity supplied by that request, so
// resolution is safe only when it maps to exactly one persisted account.
func (a *api) resolveEmptyAccountCompatibility(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Do not let an encoded separator become an alternate spelling of this
		// narrowly-scoped compatibility route after URL decoding.
		if r.URL.RawPath != "" {
			http.NotFound(w, r)
			return
		}

		vars := mux.Vars(r)
		accounts, err := a.accountRepo.GetByAPNSToken(r.Context(), vars["apns"])
		if err != nil {
			a.errorResponse(w, r, http.StatusInternalServerError, errCompatAccountLookup)
			return
		}

		switch len(accounts) {
		case 0:
			a.errorResponse(w, r, http.StatusNotFound, errCompatAccountNotFound)
			return
		case 1:
			if accounts[0].AccountID == "" {
				a.errorResponse(w, r, http.StatusInternalServerError, errCompatAccountInvalid)
				return
			}
		default:
			a.errorResponse(w, r, http.StatusConflict, errCompatAccountAmbiguous)
			return
		}

		resolvedVars := make(map[string]string, len(vars)+1)
		for key, value := range vars {
			resolvedVars[key] = value
		}
		resolvedVars["redditID"] = accounts[0].AccountID
		next(w, mux.SetURLVars(r, resolvedVars))
	}
}
