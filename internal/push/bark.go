package push

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sideshow/apns2/payload"

	"github.com/christianselig/apollo-backend/internal/domain"
)

const barkAllowedOriginsEnv = "BARK_ALLOWED_ORIGINS"

var (
	errInvalidBarkEndpoint = errors.New("invalid Bark endpoint")
	errBarkRequestFailed   = errors.New("Bark endpoint request failed")
)

// deniedBarkPrefixes is the complete deny side of the public-destination
// policy. IPv4 lists every IANA special-purpose block plus multicast and
// future-use space. IPv6 denies everything outside the currently allocated
// 2000::/3 global-unicast space, then removes the special-purpose blocks that
// live inside 2000::/3. IPv4-mapped IPv6 addresses are unmapped before this
// table is consulted.
var deniedBarkPrefixes = [...]netip.Prefix{
	// IPv4 special-purpose, private, link-local, multicast, and reserved space.
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("10.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("127.0.0.0/8"),
	netip.MustParsePrefix("169.254.0.0/16"),
	netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("192.31.196.0/24"),
	netip.MustParsePrefix("192.52.193.0/24"),
	netip.MustParsePrefix("192.88.99.0/24"),
	netip.MustParsePrefix("192.168.0.0/16"),
	netip.MustParsePrefix("192.175.48.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("224.0.0.0/4"),
	netip.MustParsePrefix("240.0.0.0/4"),

	// IPv6 outside 2000::/3 is not allocated public global-unicast space.
	netip.MustParsePrefix("::/3"),
	netip.MustParsePrefix("4000::/2"),
	netip.MustParsePrefix("8000::/1"),

	// IPv6 special-purpose blocks within 2000::/3.
	netip.MustParsePrefix("2001::/23"),
	netip.MustParsePrefix("2001:db8::/32"),
	netip.MustParsePrefix("2002::/16"),
	netip.MustParsePrefix("2620:4f:8000::/48"),
	netip.MustParsePrefix("3fff::/20"),
}

// barkResolver and barkDestinationPolicy keep Bark's registrant-supplied
// endpoint behind an explicit, operator-owned allowlist. DNS is deliberately
// resolved in DialContext, rather than only during validation, so a hostname
// cannot pass an earlier check and then rebind to an internal address.
type barkResolver interface {
	LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error)
}

type barkDestinationPolicy struct {
	allowedOrigins map[string]struct{}
	resolver       barkResolver
	dialContext    func(context.Context, string, string) (net.Conn, error)
	client         *http.Client
}

var cachedBarkPolicy struct {
	sync.Mutex
	origins     string
	policy      *barkDestinationPolicy
	err         error
	initialized bool
}

func newBarkDestinationPolicyFromEnvironment() (*barkDestinationPolicy, error) {
	origins := os.Getenv(barkAllowedOriginsEnv)
	cachedBarkPolicy.Lock()
	defer cachedBarkPolicy.Unlock()
	if cachedBarkPolicy.initialized && cachedBarkPolicy.origins == origins {
		return cachedBarkPolicy.policy, cachedBarkPolicy.err
	}

	cachedBarkPolicy.origins = origins
	cachedBarkPolicy.policy, cachedBarkPolicy.err = newBarkDestinationPolicy(origins, net.DefaultResolver, (&net.Dialer{}).DialContext)
	cachedBarkPolicy.initialized = true
	return cachedBarkPolicy.policy, cachedBarkPolicy.err
}

func newBarkDestinationPolicy(origins string, resolver barkResolver, dialContext func(context.Context, string, string) (net.Conn, error)) (*barkDestinationPolicy, error) {
	if strings.TrimSpace(origins) == "" {
		return nil, fmt.Errorf("%s must list one or more exact Bark origins", barkAllowedOriginsEnv)
	}

	p := &barkDestinationPolicy{
		allowedOrigins: make(map[string]struct{}),
		resolver:       resolver,
		dialContext:    dialContext,
	}
	for index, raw := range strings.Split(origins, ",") {
		u, err := url.Parse(strings.TrimSpace(raw))
		if err != nil {
			return nil, fmt.Errorf("invalid %s entry %d", barkAllowedOriginsEnv, index+1)
		}
		origin, err := barkOrigin(u)
		if err != nil {
			return nil, fmt.Errorf("invalid %s entry %d: %w", barkAllowedOriginsEnv, index+1, err)
		}
		if u.Path != "" && u.Path != "/" || u.RawQuery != "" || u.Fragment != "" {
			return nil, fmt.Errorf("%s entry %d must not include a path, query, or fragment", barkAllowedOriginsEnv, index+1)
		}
		p.allowedOrigins[origin] = struct{}{}
	}
	p.client = p.newHTTPClient()

	return p, nil
}

func barkOrigin(u *url.URL) (string, error) {
	if u == nil || (u.Scheme != "http" && u.Scheme != "https") {
		return "", fmt.Errorf("must use http or https")
	}
	if u.User != nil {
		return "", fmt.Errorf("must not include userinfo")
	}
	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	if host == "" {
		return "", fmt.Errorf("must include a host")
	}
	port := u.Port()
	if port == "" {
		if u.Scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	}
	portNumber, err := strconv.Atoi(port)
	if err != nil || portNumber < 1 || portNumber > 65535 || !isBarkPort(portNumber) {
		return "", fmt.Errorf("uses an unsafe port")
	}
	if ip, err := netip.ParseAddr(host); err == nil && !isSafeBarkIP(ip) {
		return "", fmt.Errorf("resolves to an unsafe IP address")
	}
	return u.Scheme + "://" + net.JoinHostPort(host, port), nil
}

func isBarkPort(port int) bool {
	// 8080 is the documented self-hosted bark-server port. Other management
	// and infrastructure ports must never become reachable through a push URL.
	return port == 80 || port == 443 || port == 8080
}

func (p *barkDestinationPolicy) validateEndpoint(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return errInvalidBarkEndpoint
	}
	origin, err := barkOrigin(u)
	if err != nil {
		// barkOrigin returns only fixed validation reasons, never URL content.
		return fmt.Errorf("%w: %v", errInvalidBarkEndpoint, err)
	}
	if _, ok := p.allowedOrigins[origin]; !ok {
		return fmt.Errorf("Bark endpoint origin is not operator-approved")
	}
	return nil
}

func (p *barkDestinationPolicy) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, fmt.Errorf("invalid Bark dial address: %w", err)
	}

	allowed := false
	for _, scheme := range []string{"http", "https"} {
		if _, ok := p.allowedOrigins[scheme+"://"+net.JoinHostPort(strings.TrimSuffix(strings.ToLower(host), "."), port)]; ok {
			allowed = true
			break
		}
	}
	if !allowed {
		return nil, fmt.Errorf("Bark dial target is not operator-approved")
	}

	ips, err := p.resolver.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return nil, fmt.Errorf("resolve Bark endpoint: %w", err)
	}
	if len(ips) == 0 {
		return nil, fmt.Errorf("Bark endpoint resolved to no addresses")
	}
	for _, ip := range ips {
		if !isSafeBarkIP(ip) {
			return nil, fmt.Errorf("Bark endpoint resolved to an unsafe IP address")
		}
	}

	// Dial a validated numeric address, never the hostname, so the system
	// resolver cannot perform a second, unchecked lookup during connection.
	return p.dialContext(ctx, network, net.JoinHostPort(ips[0].String(), port))
}

func isSafeBarkIP(addr netip.Addr) bool {
	if !addr.IsValid() || addr.Zone() != "" {
		return false
	}
	addr = addr.Unmap()
	for _, prefix := range deniedBarkPrefixes {
		if prefix.Contains(addr) {
			return false
		}
	}
	return true
}

func sanitizedBarkRequestError(err error) error {
	// Preserve cancellation semantics for callers without retaining the
	// url.Error wrapper, whose Error string contains the registrant's full Bark
	// bearer URL (device key and query parameters).
	switch {
	case errors.Is(err, context.Canceled):
		return fmt.Errorf("%w: %w", errBarkRequestFailed, context.Canceled)
	case errors.Is(err, context.DeadlineExceeded):
		return fmt.Errorf("%w: %w", errBarkRequestFailed, context.DeadlineExceeded)
	default:
		return errBarkRequestFailed
	}
}

func (p *barkDestinationPolicy) newHTTPClient() *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.DialContext = p.DialContext
	return &http.Client{
		Transport: transport,
		Timeout:   10 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

func (p *barkDestinationPolicy) httpClient() *http.Client {
	return p.client
}

// barkRequest is the JSON body bark-server accepts on POST (api.day.app or
// self-hosted). `url` is opened on notification tap and supports custom URL
// schemes, which is what deep-links back into Apollo.
type barkRequest struct {
	Title    string `json:"title,omitempty"`
	Subtitle string `json:"subtitle,omitempty"`
	Body     string `json:"body"`
	URL      string `json:"url,omitempty"`
	Group    string `json:"group,omitempty"`
	Icon     string `json:"icon,omitempty"`
	Badge    *int   `json:"badge,omitempty"`
	Level    string `json:"level,omitempty"`
	Sound    string `json:"sound,omitempty"`
}

// barkRequestFromPayload translates an APNs payload into a Bark push. The
// apns2 payload builder is the single source of truth for every notification
// this backend produces, so rather than teaching each producer about Bark,
// marshal the payload it built and lift out the alert fields plus the custom
// keys Apollo uses for tap routing. Marshals fresh on every call — the
// subreddit/user workers mutate AlertTitle on a shared payload between sends.
func barkRequestFromPayload(p *payload.Payload) (*barkRequest, error) {
	raw, err := json.Marshal(p)
	if err != nil {
		return nil, err
	}

	var m map[string]interface{}
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}

	aps, _ := m["aps"].(map[string]interface{})
	alert, _ := aps["alert"].(map[string]interface{})

	req := &barkRequest{Level: "active"}
	req.Title, _ = alert["title"].(string)
	req.Subtitle, _ = alert["subtitle"].(string)
	req.Body, _ = alert["body"].(string)

	if badge, ok := aps["badge"].(float64); ok {
		b := int(badge)
		req.Badge = &b
	}

	// Group notifications the way APNs would have threaded them.
	if tid, ok := aps["thread-id"].(string); ok && tid != "" {
		req.Group = tid
	} else if cat, ok := aps["category"].(string); ok && cat != "" {
		req.Group = cat
	} else {
		req.Group = "apollo"
	}

	// Everything outside "aps" is a custom key (post_id, subreddit, type, …).
	customs := make(map[string]interface{}, len(m))
	for k, v := range m {
		if k != "aps" {
			customs[k] = v
		}
	}

	// Reddit fills `thumbnail` with sentinels ("self", "default", "nsfw",
	// "spoiler", "image") when a post has no real thumbnail; only a URL is
	// usable as a Bark icon, and leaving Icon empty lets the default-icon
	// fallback in sendBark kick in.
	if thumb, ok := customs["thumbnail"].(string); ok && (strings.HasPrefix(thumb, "https://") || strings.HasPrefix(thumb, "http://")) {
		req.Icon = thumb
	}

	// Carry the payload's sound across, minus the file extension: Apollo's
	// pushes say "traloop.wav", and the matching Bark-side file is
	// assets/bark-sounds/traloop.caf (bark-server appends ".caf" to
	// extensionless values). Plays if the user imported that .caf into the
	// Bark app; iOS falls back to the default alert sound otherwise. Devices
	// whose push URL pins ?sound= (the tweak, mirroring Apollo's in-app
	// sound picker) override this — query beats body on bark-server.
	if sound, ok := aps["sound"].(string); ok && sound != "" && sound != "default" {
		req.Sound = strings.TrimSuffix(sound, filepath.Ext(sound))
	}

	req.URL = clickURL(customs)

	// Bark requires a body; the title alone is better than a dropped push.
	if req.Body == "" {
		req.Body = req.Title
	}
	if req.Body == "" {
		req.Body = "New notification"
	}

	return req, nil
}

// clickURL derives the apollo:// deep link opened when the Bark notification
// is tapped, from the same custom keys Apollo's own notification tap handler
// uses. Private messages have no post to open, so they land on the inbox
// (an Apollo-Reborn tweak deep link). Anything with a post lands on the
// thread — the `apollo://reddit.com/<reddit path>` form Apollo routes
// natively.
//
// When the payload names a comment (replies and mentions), the link anchors
// it with `/_/<comment_id>/?context=1` — byte-for-byte the share-link format
// Apollo itself generates, verified against the link-parser regex in the
// Apollo binary. The slug placeholder must be `_`, NOT `-`: the parser
// captures the comment id as `(\w+)` after an optional `(?:/\w+)?` slug, and
// `-` isn't a \w character, so a `/-/` link silently degrades to opening the
// post unanchored. context=1 shows the parent above the comment, matching
// what a native notification tap does.
func clickURL(customs map[string]interface{}) string {
	if t, _ := customs["type"].(string); t == "private-message" {
		return "apollo://reborn/inbox"
	}

	postID, _ := customs["post_id"].(string)
	subreddit, _ := customs["subreddit"].(string)
	if postID == "" || subreddit == "" {
		return "apollo://reborn/inbox"
	}

	if commentID, _ := customs["comment_id"].(string); commentID != "" {
		return fmt.Sprintf("apollo://reddit.com/r/%s/comments/%s/_/%s/?context=1",
			url.PathEscape(subreddit), url.PathEscape(postID), url.PathEscape(commentID))
	}

	return fmt.Sprintf("apollo://reddit.com/r/%s/comments/%s",
		url.PathEscape(subreddit), url.PathEscape(postID))
}

func (s *Sender) sendBark(ctx context.Context, device domain.Device, p *payload.Payload) (Result, error) {
	policy, err := newBarkDestinationPolicyFromEnvironment()
	if err != nil {
		return Result{}, err
	}
	return s.sendBarkWithPolicy(ctx, device, p, policy)
}

func (s *Sender) sendBarkWithPolicy(ctx context.Context, device domain.Device, p *payload.Payload, policy *barkDestinationPolicy) (Result, error) {
	if err := policy.validateEndpoint(device.TransportEndpoint); err != nil {
		return Result{}, err
	}

	req, err := barkRequestFromPayload(p)
	if err != nil {
		return Result{}, err
	}

	// No post thumbnail — show Apollo's icon rather than Bark's. (A ?icon=
	// pinned on the device's push URL overrides either; query parameters
	// beat the JSON body on bark-server.)
	if req.Icon == "" {
		req.Icon = s.barkDefaultIcon
	}

	body, err := json.Marshal(req)
	if err != nil {
		return Result{}, err
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, device.TransportEndpoint, bytes.NewReader(body))
	if err != nil {
		return Result{}, errInvalidBarkEndpoint
	}
	httpReq.Header.Set("Content-Type", "application/json; charset=utf-8")

	res, err := policy.httpClient().Do(httpReq)
	if err != nil {
		return Result{}, sanitizedBarkRequestError(err)
	}
	defer res.Body.Close()

	// bark-server answers {"code":200,"message":"success"} on delivery; a
	// 200 with a non-200 code (e.g. bad device key) is still a failure.
	respBody, _ := io.ReadAll(io.LimitReader(res.Body, 4096))
	var barkRes struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	}
	_ = json.Unmarshal(respBody, &barkRes)

	if res.StatusCode != http.StatusOK || barkRes.Code != http.StatusOK {
		// The endpoint is registrant-selected. Never propagate its response body
		// to API callers or logs: that would turn an attempted SSRF into a read
		// primitive even though the response size is capped.
		return Result{Status: res.StatusCode, Reason: "Bark endpoint rejected the notification"}, nil
	}

	return Result{Sent: true, Status: res.StatusCode}, nil
}
