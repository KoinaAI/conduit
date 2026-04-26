package pricing

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/KoinaAI/conduit/backend/internal/model"
)

const managedCatalogProfilePrefix = "pricing-catalog-models-dev-"
const maxCatalogResponseBodyBytes int64 = 4 << 20

var errCatalogResponseTooLarge = errors.New("pricing catalog response too large")

type Service struct {
	sourceURL          string
	client             *http.Client
	allowPrivateSource bool
}

// Option customises Service construction.
type Option func(*Service)

// WithAllowPrivateSourceForTests permits the pricing catalog URL to resolve to
// loopback or private IP ranges. It exists solely so unit tests can run against
// httptest servers; production code paths must never use it.
func WithAllowPrivateSourceForTests() Option {
	return func(s *Service) {
		s.allowPrivateSource = true
	}
}

type SyncResult struct {
	Profiles   []model.PricingProfile
	SourceURL  string
	FinishedAt time.Time
	Err        error
}

type catalogProvider struct {
	Models map[string]catalogModel `json:"models"`
}

type catalogModel struct {
	Cost map[string]float64 `json:"cost"`
}

func NewService(sourceURL string, client *http.Client, opts ...Option) *Service {
	if client == nil {
		client = &http.Client{Timeout: 20 * time.Second}
	}
	s := &Service{
		sourceURL: strings.TrimSpace(sourceURL),
		client:    client,
	}
	for _, opt := range opts {
		if opt != nil {
			opt(s)
		}
	}
	return s
}

// validateSourceURL enforces a small SSRF-style allowlist on the configured
// pricing catalog URL: only http/https schemes, and (unless explicitly enabled
// for tests) no loopback / private / link-local hosts.
func (s *Service) validateSourceURL() error {
	parsed, err := url.Parse(s.sourceURL)
	if err != nil {
		return fmt.Errorf("invalid pricing catalog url: %w", err)
	}
	scheme := strings.ToLower(parsed.Scheme)
	if scheme != "http" && scheme != "https" {
		return fmt.Errorf("pricing catalog url scheme %q is not allowed", parsed.Scheme)
	}
	host := parsed.Hostname()
	if host == "" {
		return errors.New("pricing catalog url is missing a host")
	}
	if s.allowPrivateSource {
		return nil
	}
	if strings.EqualFold(host, "localhost") || strings.HasSuffix(strings.ToLower(host), ".localhost") {
		return errors.New("pricing catalog url must not target localhost")
	}
	if ip := net.ParseIP(host); ip != nil {
		if blockedCatalogIP(ip) {
			return errors.New("pricing catalog url must not target private or local addresses")
		}
		return nil
	}
	ips, lookupErr := net.LookupIP(host)
	if lookupErr != nil {
		return fmt.Errorf("pricing catalog url host lookup failed: %w", lookupErr)
	}
	for _, ip := range ips {
		if blockedCatalogIP(ip) {
			return errors.New("pricing catalog url must not target private or local addresses")
		}
	}
	return nil
}

func blockedCatalogIP(ip net.IP) bool {
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified()
}

func (s *Service) Enabled() bool {
	return s != nil && strings.TrimSpace(s.sourceURL) != ""
}

func (s *Service) PrepareSync(ctx context.Context) SyncResult {
	result := SyncResult{
		SourceURL:  strings.TrimSpace(s.sourceURL),
		FinishedAt: time.Now().UTC(),
	}
	if !s.Enabled() {
		result.Err = fmt.Errorf("pricing catalog source is not configured")
		return result
	}
	profiles, err := s.FetchProfiles(ctx)
	result.Profiles = profiles
	result.Err = err
	return result
}

func (s *Service) FetchProfiles(ctx context.Context) ([]model.PricingProfile, error) {
	if !s.Enabled() {
		return nil, fmt.Errorf("pricing catalog source is not configured")
	}
	if err := s.validateSourceURL(); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.sourceURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("pricing catalog returned status %d", resp.StatusCode)
	}

	body, err := readLimitedCatalogResponseBody(resp.Body, maxCatalogResponseBodyBytes)
	if err != nil {
		if errors.Is(err, errCatalogResponseTooLarge) {
			return nil, fmt.Errorf("pricing catalog response exceeds %d bytes", maxCatalogResponseBodyBytes)
		}
		return nil, err
	}

	var payload map[string]catalogProvider
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, err
	}

	profiles := make([]model.PricingProfile, 0, len(payload))
	for providerID, provider := range payload {
		for modelName, entry := range provider.Models {
			cost := entry.Cost
			if len(cost) == 0 {
				continue
			}
			profile := model.PricingProfile{
				ID:                    managedCatalogProfileID(providerID, modelName),
				Name:                  fmt.Sprintf("models.dev / %s / %s", strings.TrimSpace(providerID), strings.TrimSpace(modelName)),
				Currency:              "USD",
				InputPerMillion:       cost["input"],
				OutputPerMillion:      cost["output"],
				CachedInputPerMillion: cost["cache_read"],
				ReasoningPerMillion:   cost["reasoning"],
			}
			if profile.InputPerMillion == 0 &&
				profile.OutputPerMillion == 0 &&
				profile.CachedInputPerMillion == 0 &&
				profile.ReasoningPerMillion == 0 {
				continue
			}
			profiles = append(profiles, profile)
		}
	}
	sort.Slice(profiles, func(i, j int) bool {
		return profiles[i].ID < profiles[j].ID
	})
	return profiles, nil
}

func readLimitedCatalogResponseBody(body io.Reader, maxBytes int64) ([]byte, error) {
	if maxBytes < 0 {
		maxBytes = 0
	}
	data, err := io.ReadAll(io.LimitReader(body, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxBytes {
		return nil, errCatalogResponseTooLarge
	}
	return data, nil
}

func ApplySyncResult(state *model.State, result SyncResult) int {
	if state == nil {
		return 0
	}

	desired := make(map[string]model.PricingProfile, len(result.Profiles))
	applied := 0
	for _, profile := range result.Profiles {
		if strings.HasPrefix(profile.ID, managedCatalogProfilePrefix) {
			desired[profile.ID] = profile
			applied++
		}
	}

	filtered := make([]model.PricingProfile, 0, len(state.PricingProfiles)+len(desired))
	for _, profile := range state.PricingProfiles {
		if !strings.HasPrefix(profile.ID, managedCatalogProfilePrefix) {
			filtered = append(filtered, profile)
			continue
		}
		if next, ok := desired[profile.ID]; ok {
			filtered = append(filtered, next)
			delete(desired, profile.ID)
		}
	}
	for _, profile := range desired {
		filtered = append(filtered, profile)
	}
	state.PricingProfiles = filtered

	validManagedIDs := make(map[string]struct{}, len(filtered))
	for _, profile := range filtered {
		if strings.HasPrefix(profile.ID, managedCatalogProfilePrefix) {
			validManagedIDs[profile.ID] = struct{}{}
		}
	}
	for index := range state.ModelRoutes {
		profileID := strings.TrimSpace(state.ModelRoutes[index].PricingProfileID)
		if !strings.HasPrefix(profileID, managedCatalogProfilePrefix) {
			continue
		}
		if _, ok := validManagedIDs[profileID]; ok {
			continue
		}
		state.ModelRoutes[index].PricingProfileID = ""
	}
	sort.Slice(state.PricingProfiles, func(i, j int) bool {
		return state.PricingProfiles[i].ID < state.PricingProfiles[j].ID
	})
	return applied
}

func managedCatalogProfileID(providerID, modelName string) string {
	slug := strings.ToLower(strings.TrimSpace(providerID + "-" + modelName))
	replacer := strings.NewReplacer(
		"/", "-",
		"\\", "-",
		" ", "-",
		":", "-",
		".", "-",
		"_", "-",
	)
	slug = replacer.Replace(slug)
	for strings.Contains(slug, "--") {
		slug = strings.ReplaceAll(slug, "--", "-")
	}
	slug = strings.Trim(slug, "-")
	if slug == "" {
		slug = "model"
	}
	return managedCatalogProfilePrefix + slug
}
