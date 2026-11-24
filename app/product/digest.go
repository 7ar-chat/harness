package product

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/7ar-chat/harness/core/notify"
	"github.com/7ar-chat/harness/core/projdata"
	"github.com/7ar-chat/harness/core/store"
)

// The weekly digest: once a week, at the hour the project's owner chose in the zone they chose, the project's
// insights report of the last seven days is generated, kept (with the report itself, so the link keeps working) and
// announced through the notification rules as the event digest.ready. A project with no activity in the week gets no
// digest. Without a ProjectData store these routes and the job do not exist.
//
//	GET  /api/projects/{pid}/digest              → DigestView: the setting, the next time, the history   (project viewer+)
//	PUT  /api/projects/{pid}/digest {enabled,weekday,hour,timezone} → DigestView                          (owner)
//	POST /api/projects/{pid}/digest/send {force?} → DigestSendResult: generate one now (owner). A week without
//	     activity answers 409 quiet_week unless force is true: the person is asked, and "Send anyway" passes force.
//	GET  /api/projects/{pid}/digest/{id}/report[?download=1]  → the stored report page                      (project viewer+)
//
// Replicas: every replica runs the job, and a digest is generated only by the replica that wins the store's claim of
// the (project, slot) pair, so two replicas never send it twice.

// DigestSetting is the digest setting as the page reads and writes it.
type DigestSetting struct {
	Enabled  bool   `json:"enabled"`
	Weekday  int    `json:"weekday"`
	Hour     int    `json:"hour"`
	Timezone string `json:"timezone"`
	// NextAt is when the next scheduled digest goes out (empty while off); LastSentAt when the last one was generated.
	NextAt     *time.Time `json:"nextAt,omitempty"`
	LastSentAt *time.Time `json:"lastSentAt,omitempty"`
}

// DigestItem is one digest of the history.
type DigestItem struct {
	ID        string    `json:"id"`
	CreatedAt time.Time `json:"createdAt"`
	From      time.Time `json:"from"`
	To        time.Time `json:"to"`
	Summary   string    `json:"summary"`
	// Trigger is "schedule" or "manual".
	Trigger string `json:"trigger"`
	// URL is where the report is served (the session authenticates the download).
	URL string `json:"url"`
}

// DigestView is the Insights tab's digest card.
type DigestView struct {
	Setting DigestSetting `json:"setting"`
	Digests []DigestItem  `json:"digests"`
	// CanEdit is true for the project's owner: only they change the setting and send one now.
	CanEdit bool `json:"canEdit"`
	// Notified says whether the project routes digest.ready to a channel of its own besides the app.
	Notified bool `json:"notified"`
}

// DigestSendResult is the answer of "send one now".
type DigestSendResult struct {
	Digest DigestItem `json:"digest"`
}

// ProjectDataOptions configures what core/projdata holds: the weekly digest and the credentials of connected TAPD
// projects.
type ProjectDataOptions struct {
	Store projdata.Store
	// TapdAllowPrivate lets a TAPD base URL point at a private or loopback address (a TAPD deployment inside the
	// company network, a test). The operator opts in (HARNESS_TAPD_ALLOW_PRIVATE=1); a user cannot.
	TapdAllowPrivate bool
	// NoJob keeps the digest job from starting (tests drive digestTick themselves).
	NoJob bool
	// Now is the clock of the digest job (nil: the insights clock, which is time.Now unless a test sets one).
	Now func() time.Time
	// onInstall hands the server to a test.
	onInstall func(*server)
}

const (
	digestTick       = time.Minute
	digestHistory    = 26
	digestManualWait = 3 * time.Second
	digestMaxPeople  = 200
)

type digestState struct {
	mu         sync.Mutex
	lastManual map[string]time.Time
}

func (s *server) digestStore() projdata.Store {
	if s.opt.ProjectData == nil {
		return nil
	}
	return s.opt.ProjectData.Store
}

func (s *server) digestNow() time.Time {
	if o := s.opt.ProjectData; o != nil && o.Now != nil {
		return o.Now().UTC()
	}
	return s.insightsNow()
}

func (s *server) routeDigest() {
	if s.digestStore() == nil {
		return
	}
	s.dg = &digestState{lastManual: map[string]time.Time{}}
	s.Route("GET /projects/{pid}/digest", s.getDigest)
	s.Route("PUT /projects/{pid}/digest", s.putDigest)
	s.Route("POST /projects/{pid}/digest/send", s.sendDigest)
	s.Route("GET /projects/{pid}/digest/{id}/report", s.digestReport)
	if f := s.opt.ProjectData.onInstall; f != nil {
		f(s)
	}
	if !s.opt.ProjectData.NoJob {
		go s.digestLoop()
	}
}

func (s *server) digestLoop() {
	t := time.NewTicker(digestTick)
	defer t.Stop()
	for range t.C {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		s.digestTick(ctx, s.digestNow())
		cancel()
	}
}

// digestTick sends every digest that is due at now. It is safe to run on several replicas at once.
func (s *server) digestTick(ctx context.Context, now time.Time) {
	st := s.digestStore()
	if st == nil || s.team == nil {
		return
	}
	all, err := st.EnabledDigests(ctx)
	if err != nil {
		slog.Warn("digest: listing the enabled projects failed", "error", err)
		return
	}
	for _, set := range all {
		slot, due := set.Due(now)
		if !due {
			continue
		}
		won, err := st.ClaimSlot(ctx, set.ProjectID, slot.Key, now)
		if err != nil {
			slog.Warn("digest: claiming the slot failed", "project", set.ProjectID, "error", err)
			continue
		}
		if !won {
			continue // another replica sends it
		}
		p, err := s.team.GetProject(ctx, set.ProjectID)
		if err != nil {
			continue // the project is gone; the claim stays, nothing is sent
		}
		d, err := s.generateDigest(ctx, p, set, projdata.TriggerSchedule, slot.Key, slot.From, slot.At, slot.Loc, false, now)
		switch {
		case err != nil:
			slog.Warn("digest: generating failed", "project", p.ID, "error", err)
			_ = st.ReleaseSlot(ctx, set.ProjectID, slot.Key) // the next tick tries again, within the grace
		case d == nil: // no activity: the week is handled, nothing is sent
			set.LastSlot = slot.Key
			s.saveDigestSettings(ctx, set)
		}
	}
}

func (s *server) saveDigestSettings(ctx context.Context, set projdata.DigestSettings) {
	// Re-read: the owner may have changed the setting while the digest was generated; keep their choice.
	cur, err := s.digestStore().GetDigestSettings(ctx, set.ProjectID)
	if err != nil {
		return
	}
	cur.LastSlot, cur.LastSentAt = set.LastSlot, set.LastSentAt
	if err := s.digestStore().PutDigestSettings(ctx, cur); err != nil {
		slog.Warn("digest: saving the setting failed", "project", set.ProjectID, "error", err)
	}
}

// generateDigest builds, stores and announces one digest. It returns nil, nil for a week without activity unless
// force is set (an owner confirmed a quiet-week digest). The weekly job never forces.
func (s *server) generateDigest(ctx context.Context, p *store.Project, set projdata.DigestSettings, trigger, slotKey string, from, to time.Time, loc *time.Location, force bool, now time.Time) (*projdata.Digest, error) {
	res, err := s.computeInsights(ctx, p.ID, "", from, to, loc)
	if err != nil {
		return nil, err
	}
	if res.Empty && !force {
		return nil, nil
	}
	page, err := RenderInsightsHTML(p.Name, res, loc)
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(res)
	if err != nil {
		return nil, err
	}
	d := projdata.Digest{ID: "dg_" + uuid.NewString(), ProjectID: p.ID, Slot: slotKey, Trigger: trigger, From: from, To: to, CreatedAt: now,
		Summary: res.Summary, HTML: page, JSON: raw}
	if err := s.digestStore().AddDigest(ctx, d); err != nil {
		return nil, err
	}
	set.LastSentAt = now
	if trigger == projdata.TriggerSchedule {
		set.LastSlot = slotKey
	}
	s.saveDigestSettings(ctx, set)
	s.announceDigest(ctx, p, d)
	return &d, nil
}

func digestReportPath(project, id string) string {
	return "/api/projects/" + project + "/digest/" + id + "/report"
}

// announceDigest emits digest.ready once for the project (its channels, as the rules say) and once for each member
// (where their own preferences send it, the app by default), and puts it in the project's live feed.
func (s *server) announceDigest(ctx context.Context, p *store.Project, d projdata.Digest) {
	title := "Weekly digest: " + p.Name
	link := s.appURL(digestReportPath(p.ID, d.ID))
	data := map[string]any{"digestId": d.ID, "projectId": p.ID}
	if s.nf != nil {
		s.nf.d.Emit(ctx, notify.Event{Key: "digest:" + p.ID + ":" + d.ID, Kind: notify.DigestReady, ProjectID: p.ID, ProjectName: p.Name,
			Title: title, Body: d.Summary, URL: link, Data: data, Window: time.Hour})
		if members, err := s.team.ListMembers(ctx, p.ID); err == nil {
			for i, m := range members {
				if i >= digestMaxPeople {
					break
				}
				s.nf.d.Emit(ctx, notify.Event{Key: "digest:" + p.ID + ":" + d.ID + ":" + m.Login, Kind: notify.DigestReady, ProjectName: p.Name,
					Title: title, Body: d.Summary, URL: link, Login: m.Login, Data: data, Window: time.Hour})
			}
		}
	}
	s.hub.publish(FeedEvent{Type: "digest.ready", ProjectID: p.ID, Actor: "system"})
}

func digestItem(project string, d projdata.Digest) DigestItem {
	return DigestItem{ID: d.ID, CreatedAt: d.CreatedAt, From: d.From, To: d.To, Summary: d.Summary, Trigger: d.Trigger, URL: digestReportPath(project, d.ID)}
}

func (s *server) digestView(ctx context.Context, p *store.Project, role string) (DigestView, error) {
	set, err := s.digestStore().GetDigestSettings(ctx, p.ID)
	if err != nil {
		return DigestView{}, err
	}
	list, err := s.digestStore().ListDigests(ctx, p.ID, digestHistory)
	if err != nil {
		return DigestView{}, err
	}
	v := DigestView{Setting: DigestSetting{Enabled: set.Enabled, Weekday: set.Weekday, Hour: set.Hour, Timezone: set.Timezone}, Digests: []DigestItem{}, CanEdit: role == store.RoleOwner}
	if v.Setting.Timezone == "" {
		v.Setting.Timezone = "UTC"
	}
	if next, ok := set.Next(s.digestNow()); ok {
		v.Setting.NextAt = &next
	}
	if !set.LastSentAt.IsZero() {
		t := set.LastSentAt
		v.Setting.LastSentAt = &t
	}
	for _, d := range list {
		v.Digests = append(v.Digests, digestItem(p.ID, d))
	}
	if s.nf != nil {
		if rules, err := s.nf.st.GetRules(ctx, p.ID); err == nil {
			v.Notified = len(rules.RuleFor(notify.DigestReady).Channels) > 0
		}
	}
	return v, nil
}

func (s *server) getDigest(w http.ResponseWriter, r *http.Request, c Caller) {
	p, role, ok := s.project(w, r, c, store.RoleViewer)
	if !ok {
		return
	}
	v, err := s.digestView(r.Context(), p, role)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (s *server) putDigest(w http.ResponseWriter, r *http.Request, c Caller) {
	p, role, ok := s.project(w, r, c, store.RoleOwner)
	if !ok {
		return
	}
	var b struct {
		Enabled  bool   `json:"enabled"`
		Weekday  int    `json:"weekday"`
		Hour     int    `json:"hour"`
		Timezone string `json:"timezone"`
	}
	if !decode(w, r, &b) {
		return
	}
	cur, err := s.digestStore().GetDigestSettings(r.Context(), p.ID)
	if err != nil {
		fail(w, err)
		return
	}
	next := cur
	next.ProjectID, next.Enabled, next.Weekday, next.Hour, next.Timezone = p.ID, b.Enabled, b.Weekday, b.Hour, b.Timezone
	if next.Timezone == "UTC" {
		next.Timezone = ""
	}
	if err := next.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if next.Enabled && (!cur.Enabled || cur.Weekday != next.Weekday || cur.Hour != next.Hour || cur.Timezone != next.Timezone) {
		// A slot earlier than this moment is never sent: switching the digest on does not send last Monday's.
		next.EnabledAt = s.digestNow()
	}
	if err := s.digestStore().PutDigestSettings(r.Context(), next); err != nil {
		fail(w, err)
		return
	}
	s.note(inProject(c, p.ID), "", "digest.settings", map[string]any{"enabled": next.Enabled, "weekday": next.Weekday, "hour": next.Hour, "timezone": next.Timezone}, FeedEvent{Type: "project.changed"})
	v, err := s.digestView(r.Context(), p, role)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (s *server) sendDigest(w http.ResponseWriter, r *http.Request, c Caller) {
	p, _, ok := s.project(w, r, c, store.RoleOwner)
	if !ok {
		return
	}
	var body struct {
		Force bool `json:"force"`
	}
	if !decode(w, r, &body) {
		return
	}
	now := s.digestNow()
	s.dg.mu.Lock()
	if last := s.dg.lastManual[p.ID]; now.Sub(last) < digestManualWait && !last.After(now) {
		s.dg.mu.Unlock()
		writeError(w, http.StatusTooManyRequests, "too_many_requests", "A digest was just sent; wait a moment.")
		return
	}
	s.dg.lastManual[p.ID] = now
	s.dg.mu.Unlock()
	set, err := s.digestStore().GetDigestSettings(r.Context(), p.ID)
	if err != nil {
		fail(w, err)
		return
	}
	from, to, loc := set.ManualRange(now)
	d, err := s.generateDigest(r.Context(), p, set, projdata.TriggerManual, "manual:"+uuid.NewString(), from, to, loc, body.Force, now)
	if err == nil && d == nil {
		// A quiet week: nothing was sent, so the rate limit is not spent; the web asks the person and asks again with force.
		s.dg.mu.Lock()
		delete(s.dg.lastManual, p.ID)
		s.dg.mu.Unlock()
		writeError(w, http.StatusConflict, "quiet_week", fmt.Sprintf("Nothing happened in the last %d days. Send a short 'quiet week' digest anyway?", int(to.Sub(from).Hours()/24+0.5)))
		return
	}
	if err != nil {
		slog.Warn("digest: sending one now failed", "project", p.ID, "error", err)
		writeError(w, http.StatusInternalServerError, "digest_failed", "The digest could not be generated.")
		return
	}
	s.note(inProject(c, p.ID), "", "digest.send", map[string]any{"digest": d.ID}, FeedEvent{Type: "project.changed"})
	writeJSON(w, http.StatusOK, DigestSendResult{Digest: digestItem(p.ID, *d)})
}

func (s *server) digestReport(w http.ResponseWriter, r *http.Request, c Caller) {
	p, _, ok := s.project(w, r, c, store.RoleViewer)
	if !ok {
		return
	}
	d, err := s.digestStore().GetDigest(r.Context(), p.ID, r.PathValue("id"))
	if errors.Is(err, projdata.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "No such digest.")
		return
	}
	if err != nil {
		fail(w, err)
		return
	}
	h := w.Header()
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Cache-Control", "private, no-store")
	h.Set("Content-Type", "text/html; charset=utf-8")
	// The page is static: nothing may run in it or be fetched by it.
	h.Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; img-src data:; sandbox")
	if r.URL.Query().Get("download") != "" {
		name := fmt.Sprintf("digest-%s-%s.html", slug(p.Name), d.CreatedAt.UTC().Format("2006-01-02"))
		h.Set("Content-Disposition", `attachment; filename="`+name+`"`)
	}
	_, _ = w.Write(d.HTML)
}

// dropProjectData removes the digest and the credentials of a deleted project.
func (s *server) dropProjectData(ctx context.Context, project string) {
	if st := s.digestStore(); st != nil {
		if err := st.DeleteProject(ctx, project); err != nil {
			slog.Warn("project data: removing a deleted project's data failed", "project", project, "error", err)
		}
	}
}
