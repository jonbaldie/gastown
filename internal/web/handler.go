package web

import (
	"bytes"
	"context"
	"crypto/rand"
	"embed"
	"encoding/hex"
	"html/template"
	"io/fs"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/jonbaldie/gastown/internal/config"
)

//go:embed static
var staticFiles embed.FS

// ConvoyFetcher defines the interface for fetching convoy data.
type ConvoyFetcher interface {
	FetchConvoys() ([]ConvoyRow, error)
	FetchMergeQueue() ([]MergeQueueRow, error)
	FetchWorkers() ([]WorkerRow, error)
	FetchMail() ([]MailRow, error)
	FetchRigs() ([]RigRow, error)
	FetchDogs() ([]DogRow, error)
	FetchEscalations() ([]EscalationRow, error)
	FetchHealth() (*HealthRow, error)
	FetchQueues() ([]QueueRow, error)
	FetchSessions() ([]SessionRow, error)
	FetchHooks() ([]HookRow, error)
	FetchMayor() (*MayorStatus, error)
	FetchIssues() ([]IssueRow, error)
	FetchActivity() ([]ActivityRow, error)
}

// expandCacheEntry holds a cached expanded-view response.
type expandCacheEntry struct {
	body []byte
	time time.Time
}

// ConvoyHandler handles HTTP requests for the convoy dashboard.
type ConvoyHandler struct {
	fetcher      ConvoyFetcher
	template     *template.Template
	fetchTimeout time.Duration
	csrfToken    string

	// Response cache: prevents cascading bd process storms when multiple
	// browser tabs or htmx auto-refresh requests arrive faster than fetches
	// complete. See GH#2618.
	cacheMu    sync.Mutex
	cacheBody  []byte
	cacheTime  time.Time
	cacheTTL   time.Duration
	cacheInUse sync.Mutex // serializes concurrent fetches (only one runs at a time)

	// Expanded-view cache: expanded views previously bypassed the response
	// cache entirely, allowing process storms via repeated ?expand= requests.
	// See GH#3117.
	expandCacheMu sync.Mutex
	expandCache   map[string]expandCacheEntry
}

// defaultCacheTTL is the minimum interval between full dashboard fetches.
// Requests arriving within this window get the cached response.
const defaultCacheTTL = 10 * time.Second

// NewConvoyHandler creates a new convoy handler with the given fetcher, fetch timeout, and CSRF token.
func NewConvoyHandler(fetcher ConvoyFetcher, fetchTimeout time.Duration, csrfToken string) (*ConvoyHandler, error) {
	tmpl, err := LoadTemplates()
	if err != nil {
		return nil, err
	}

	return &ConvoyHandler{
		fetcher:      fetcher,
		template:     tmpl,
		fetchTimeout: fetchTimeout,
		csrfToken:    csrfToken,
		cacheTTL:     defaultCacheTTL,
	}, nil
}

// ServeHTTP handles GET / requests and renders the convoy dashboard.
// Uses a response cache to prevent bd process storms from overlapping
// requests (htmx auto-refresh, multiple tabs). Only one fetch cycle
// runs at a time; concurrent requests get the cached response.
func (h *ConvoyHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	expandPanel := r.URL.Query().Get("expand")
	if h.serveCached(w, expandPanel) {
		return
	}
	h.cacheInUse.Lock()
	defer h.cacheInUse.Unlock()
	if h.serveCached(w, expandPanel) {
		return
	}
	body := h.fetchAndRender(r, expandPanel)
	if body == nil {
		http.Error(w, "Failed to render template", http.StatusInternalServerError)
		return
	}
	h.storeCached(expandPanel, body)
	writeDashboardHTML(w, body, "response")
}

func (h *ConvoyHandler) serveCached(w http.ResponseWriter, panel string) bool {
	body, ok := h.cachedBody(panel)
	if !ok {
		return false
	}
	writeDashboardHTML(w, body, "cached response")
	return true
}

func (h *ConvoyHandler) cachedBody(panel string) ([]byte, bool) {
	if panel == "" {
		h.cacheMu.Lock()
		defer h.cacheMu.Unlock()
		return h.cacheBody, len(h.cacheBody) > 0 && time.Since(h.cacheTime) < h.cacheTTL
	}
	h.expandCacheMu.Lock()
	defer h.expandCacheMu.Unlock()
	entry, ok := h.expandCache[panel]
	return entry.body, ok && time.Since(entry.time) < h.cacheTTL
}

func (h *ConvoyHandler) storeCached(panel string, body []byte) {
	if panel == "" {
		h.cacheMu.Lock()
		h.cacheBody = body
		h.cacheTime = time.Now()
		h.cacheMu.Unlock()
	} else {
		h.expandCacheMu.Lock()
		if h.expandCache == nil {
			h.expandCache = make(map[string]expandCacheEntry)
		}
		h.expandCache[panel] = expandCacheEntry{body: body, time: time.Now()}
		h.expandCacheMu.Unlock()
	}
}

func writeDashboardHTML(w http.ResponseWriter, body []byte, operation string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if _, err := w.Write(body); err != nil {
		log.Printf("dashboard: %s write failed: %v", operation, err)
	}
}

// fetchAndRender runs all 14 fetchers in parallel and renders the template.
// Returns the rendered HTML bytes, or nil on template error.
func (h *ConvoyHandler) fetchAndRender(r *http.Request, expandPanel string) []byte {
	ctx, cancel := context.WithTimeout(r.Context(), h.fetchTimeout)
	defer cancel()
	snapshot := fetchDashboardSnapshot(ctx, h.fetcher)
	data := snapshot.convoyData(expandPanel, h.csrfToken)
	var buf bytes.Buffer
	if err := h.template.ExecuteTemplate(&buf, "convoy.html", data); err != nil {
		log.Printf("dashboard: template execution failed: %v", err)
		return nil
	}

	return buf.Bytes()
}

type dashboardSnapshot struct {
	overview   dashboardOverview
	operations dashboardOperations
}

type dashboardOverview struct {
	convoys     []ConvoyRow
	mergeQueue  []MergeQueueRow
	workers     []WorkerRow
	mail        []MailRow
	rigs        []RigRow
	dogs        []DogRow
	escalations []EscalationRow
	activity    []ActivityRow
}

type dashboardOperations struct {
	health   *HealthRow
	queues   []QueueRow
	sessions []SessionRow
	hooks    []HookRow
	mayor    *MayorStatus
	issues   []IssueRow
}

func fetchDashboardSnapshot(ctx context.Context, fetcher ConvoyFetcher) dashboardSnapshot {
	var result dashboardSnapshot
	var wg sync.WaitGroup
	tasks := []struct {
		name string
		run  func() error
	}{
		{"FetchConvoys", func() (err error) { result.overview.convoys, err = fetcher.FetchConvoys(); return err }},
		{"FetchMergeQueue", func() (err error) { result.overview.mergeQueue, err = fetcher.FetchMergeQueue(); return err }},
		{"FetchWorkers", func() (err error) { result.overview.workers, err = fetcher.FetchWorkers(); return err }},
		{"FetchMail", func() (err error) { result.overview.mail, err = fetcher.FetchMail(); return err }},
		{"FetchRigs", func() (err error) { result.overview.rigs, err = fetcher.FetchRigs(); return err }},
		{"FetchDogs", func() (err error) { result.overview.dogs, err = fetcher.FetchDogs(); return err }},
		{"FetchEscalations", func() (err error) { result.overview.escalations, err = fetcher.FetchEscalations(); return err }},
		{"FetchHealth", func() (err error) { result.operations.health, err = fetcher.FetchHealth(); return err }},
		{"FetchQueues", func() (err error) { result.operations.queues, err = fetcher.FetchQueues(); return err }},
		{"FetchSessions", func() (err error) { result.operations.sessions, err = fetcher.FetchSessions(); return err }},
		{"FetchHooks", func() (err error) { result.operations.hooks, err = fetcher.FetchHooks(); return err }},
		{"FetchMayor", func() (err error) { result.operations.mayor, err = fetcher.FetchMayor(); return err }},
		{"FetchIssues", func() (err error) { result.operations.issues, err = fetcher.FetchIssues(); return err }},
		{"FetchActivity", func() (err error) { result.overview.activity, err = fetcher.FetchActivity(); return err }},
	}
	for _, task := range tasks {
		wg.Add(1)
		go runDashboardFetch(&wg, task.name, task.run)
	}
	waitForDashboardFetches(ctx, &wg)
	return result
}

func runDashboardFetch(wg *sync.WaitGroup, name string, run func() error) {
	defer wg.Done()
	if err := run(); err != nil {
		log.Printf("dashboard: %s failed: %v", name, err)
	}
}

func waitForDashboardFetches(ctx context.Context, wg *sync.WaitGroup) {
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
		log.Printf("dashboard: fetch timeout")
		<-done
	}
}

func (s dashboardSnapshot) convoyData(expandPanel, csrfToken string) ConvoyData {
	overview, operations := s.overview, s.operations
	return ConvoyData{
		Convoys: overview.convoys, MergeQueue: overview.mergeQueue, Workers: overview.workers, Mail: overview.mail,
		Rigs: overview.rigs, Dogs: overview.dogs, Escalations: overview.escalations, Activity: overview.activity,
		ConvoyWorkData: ConvoyWorkData{Queues: operations.queues, Sessions: operations.sessions, Hooks: operations.hooks, Issues: enrichIssuesWithAssignees(operations.issues, operations.hooks)},
		ConvoyViewState: ConvoyViewState{
			Health: operations.health, Mayor: operations.mayor,
			Summary: computeSummary(overview.workers, operations.hooks, operations.issues, overview.convoys, overview.escalations, overview.activity),
			Expand:  expandPanel, CSRFToken: csrfToken,
		},
	}
}

// computeSummary calculates dashboard stats and alerts from fetched data.
func computeSummary(workers []WorkerRow, hooks []HookRow, issues []IssueRow,
	convoys []ConvoyRow, escalations []EscalationRow, activity []ActivityRow) *DashboardSummary {

	summary := &DashboardSummary{
		PolecatCount:    len(workers),
		HookCount:       len(hooks),
		IssueCount:      len(issues),
		ConvoyCount:     len(convoys),
		EscalationCount: len(escalations),
	}

	summary.StuckPolecats = countStuckPolecats(workers)
	summary.StaleHooks = countStaleHooks(hooks)
	summary.UnackedEscalations = countUnackedEscalations(escalations)
	summary.HighPriorityIssues = countHighPriorityIssues(issues)
	summary.DeadSessions = countDeadSessions(activity)
	summary.HasAlerts = dashboardHasAlerts(summary)

	return summary
}

func countStuckPolecats(workers []WorkerRow) int {
	count := 0
	for _, worker := range workers {
		if worker.WorkStatus == "stuck" {
			count++
		}
	}
	return count
}

func countStaleHooks(hooks []HookRow) int {
	count := 0
	for _, hook := range hooks {
		if hook.IsStale {
			count++
		}
	}
	return count
}

func countUnackedEscalations(escalations []EscalationRow) int {
	count := 0
	for _, escalation := range escalations {
		if !escalation.Acked {
			count++
		}
	}
	return count
}

func countHighPriorityIssues(issues []IssueRow) int {
	count := 0
	for _, issue := range issues {
		if issue.Priority == 1 || issue.Priority == 2 {
			count++
		}
	}
	return count
}

func countDeadSessions(activity []ActivityRow) int {
	count := 0
	for _, event := range activity {
		if event.Type == "session_death" || event.Type == "mass_death" {
			count++
		}
	}
	return count
}

func dashboardHasAlerts(summary *DashboardSummary) bool {
	return summary.StuckPolecats > 0 || summary.StaleHooks > 0 ||
		summary.UnackedEscalations > 0 || summary.DeadSessions > 0 ||
		summary.HighPriorityIssues > 0
}

// enrichIssuesWithAssignees adds Assignee info to issues by cross-referencing hooks.
func enrichIssuesWithAssignees(issues []IssueRow, hooks []HookRow) []IssueRow {
	// Build a map of issue ID -> assignee from hooks
	hookMap := make(map[string]string)
	for _, hook := range hooks {
		hookMap[hook.ID] = hook.Agent
	}

	// Enrich issues with assignee info
	for i := range issues {
		if assignee, ok := hookMap[issues[i].ID]; ok {
			issues[i].Assignee = assignee
		}
	}
	return issues
}

// generateCSRFToken creates a cryptographically random token for CSRF protection.
func generateCSRFToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		log.Fatalf("failed to generate CSRF token: %v", err)
	}
	return hex.EncodeToString(b)
}

// NewDashboardMux creates an HTTP handler that serves both the dashboard and API.
// webCfg may be nil, in which case defaults are used.
func NewDashboardMux(fetcher ConvoyFetcher, webCfg *config.WebTimeoutsConfig) (http.Handler, error) {
	if webCfg == nil {
		webCfg = config.DefaultWebTimeoutsConfig()
	}

	csrfToken := generateCSRFToken()

	fetchTimeout := config.ParseDurationOrDefault(webCfg.FetchTimeout, 8*time.Second)
	convoyHandler, err := NewConvoyHandler(fetcher, fetchTimeout, csrfToken)
	if err != nil {
		return nil, err
	}

	defaultRunTimeout := config.ParseDurationOrDefault(webCfg.DefaultRunTimeout, 30*time.Second)
	maxRunTimeout := config.ParseDurationOrDefault(webCfg.MaxRunTimeout, 60*time.Second)
	apiHandler := NewAPIHandler(defaultRunTimeout, maxRunTimeout, csrfToken)

	// Create static file server from embedded files
	staticFS, err := fs.Sub(staticFiles, "static")
	if err != nil {
		return nil, err
	}
	staticHandler := http.FileServer(http.FS(staticFS))

	mux := http.NewServeMux()
	mux.Handle("/api/", apiHandler)
	mux.Handle("/static/", http.StripPrefix("/static/", staticHandler))
	mux.Handle("/", convoyHandler)

	return mux, nil
}
