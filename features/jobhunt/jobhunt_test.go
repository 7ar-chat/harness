package jobhunt

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const ghBoard = `{"jobs":[
 {"id":101,"title":"Senior Go Engineer","absolute_url":"https://boards.greenhouse.io/acme/jobs/101","updated_at":"2026-10-01T10:00:00-04:00","location":{"name":"Paris, France"}},
 {"id":102,"title":"Go Engineering Intern","absolute_url":"https://boards.greenhouse.io/acme/jobs/102","location":{"name":"Paris"}},
 {"id":103,"title":"Staff Go Engineer","absolute_url":"https://boards.greenhouse.io/acme/jobs/103","location":{"name":"New York"}},
 {"id":104,"title":"Account Executive","absolute_url":"https://boards.greenhouse.io/acme/jobs/104","location":{"name":"Paris"}}]}`

const leverBoard = `[{"id":"l1","text":"Backend Engineer (Go)","hostedUrl":"https://jobs.lever.co/globex/l1","createdAt":1760000000000,"workplaceType":"remote","categories":{"location":"Anywhere"}}]`

const ashbyBoard = `{"jobs":[{"id":"a1","title":"Go Platform Engineer","location":"Berlin","isRemote":false,"jobUrl":"https://jobs.ashbyhq.com/initech/a1","publishedAt":"2026-10-05T00:00:00.000+00:00"},
 {"id":"a2","title":"Go Hidden","location":"Berlin","jobUrl":"https://jobs.ashbyhq.com/initech/a2","isListed":false}]}`

func newSvc(t *testing.T) (*Service, *map[string]string, *time.Time) {
	t.Helper()
	boards := map[string]string{
		"/v1/boards/acme/jobs":           ghBoard,
		"/v0/postings/globex":            leverBoard,
		"/posting-api/job-board/initech": ashbyBoard,
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, ok := boards[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	s := New(t.TempDir())
	s.Bases = map[string]string{Greenhouse: srv.URL, Lever: srv.URL, Ashby: srv.URL}
	s.Now = func() time.Time { return now }
	return s, &boards, &now
}

func setup(t *testing.T, s *Service, edit func(*Profile)) {
	t.Helper()
	p := Profile{Enabled: true, Titles: []string{"go "}, Exclude: []string{"intern"}, Locations: []string{"Paris", "remote", "Berlin"},
		Companies: []Company{{Name: "Acme", ATS: Greenhouse, Slug: "acme"}, {Name: "Globex", ATS: Lever, Slug: "globex"}, {Name: "Initech", ATS: Ashby, Slug: "initech"}}}
	if edit != nil {
		edit(&p)
	}
	if _, err := s.SetProfile("ann", p); err != nil {
		t.Fatal(err)
	}
}

func TestScanKeepsTheJobsThatFitTheProfile(t *testing.T) {
	// Arrange
	s, _, _ := newSvc(t)
	setup(t, s, nil)
	// Act
	res, err := s.Scan(context.Background(), "ann")
	// Assert
	if err != nil {
		t.Fatal(err)
	}
	titles := map[string]bool{}
	for _, j := range res.New {
		titles[j.Title] = true
	}
	// the intern, the New York job, the sales job and the unlisted one are out; the remote lever job is in
	for _, want := range []string{"Senior Go Engineer", "Backend Engineer (Go)", "Go Platform Engineer"} {
		if !titles[want] {
			t.Errorf("missing %q in %v", want, titles)
		}
	}
	if res.Added != 3 || res.Boards != 3 || res.Fetched != 6 {
		t.Fatalf("%+v", res)
	}
}

func TestScanAddsAJobOnlyOnceAndClosesTheOnesThatLeftTheBoard(t *testing.T) {
	// Arrange
	s, boards, _ := newSvc(t)
	setup(t, s, nil)
	if _, err := s.Scan(context.Background(), "ann"); err != nil {
		t.Fatal(err)
	}
	// Act: the Lever board empties
	(*boards)["/v0/postings/globex"] = `[]`
	res, err := s.Scan(context.Background(), "ann")
	// Assert
	if err != nil || res.Added != 0 || res.Closed != 1 {
		t.Fatalf("%+v %v", res, err)
	}
	if c, _ := s.Counts("ann"); c[StatusNew] != 2 || c[StatusClosed] != 1 {
		t.Fatalf("%v", c)
	}
}

func TestScanReportsABrokenBoardAndKeepsTheOthers(t *testing.T) {
	// Arrange
	s, _, _ := newSvc(t)
	setup(t, s, func(p *Profile) { p.Companies = append(p.Companies, Company{ATS: Lever, Slug: "gone"}) })
	// Act
	res, err := s.Scan(context.Background(), "ann")
	// Assert
	if err != nil || res.Added != 3 || len(res.Errors) != 1 || !strings.Contains(res.Errors[0], "404") {
		t.Fatalf("%+v %v", res, err)
	}
}

func TestScanSkipsBlacklistedCompaniesAndDomains(t *testing.T) {
	for _, bl := range []string{"ACME", "acme", "boards.greenhouse.io"} {
		s, _, _ := newSvc(t)
		setup(t, s, func(p *Profile) { p.Blacklist = []string{bl} })
		res, err := s.Scan(context.Background(), "ann")
		if err != nil || res.Added != 2 {
			t.Fatalf("blacklist %q: %+v %v", bl, res, err)
		}
	}
}

func TestRemoteOnlyKeepsOnlyRemoteJobs(t *testing.T) {
	s, _, _ := newSvc(t)
	setup(t, s, func(p *Profile) { p.RemoteOnly = true })
	res, err := s.Scan(context.Background(), "ann")
	if err != nil || res.Added != 1 || res.New[0].Company != "Globex" {
		t.Fatalf("%+v %v", res, err)
	}
}

func TestScanNeedsRolesAndBoards(t *testing.T) {
	s, _, _ := newSvc(t)
	if _, err := s.Scan(context.Background(), "ann"); err == nil {
		t.Fatal("no titles")
	}
	if _, err := s.SetProfile("ann", Profile{Titles: []string{"go"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Scan(context.Background(), "ann"); err == nil {
		t.Fatal("no companies")
	}
}

func scanned(t *testing.T, edit func(*Profile)) (*Service, *time.Time, map[string]Job) {
	t.Helper()
	s, _, now := newSvc(t)
	setup(t, s, edit)
	if _, err := s.Scan(context.Background(), "ann"); err != nil {
		t.Fatal(err)
	}
	by := map[string]Job{}
	jobs, _ := s.List("ann", "all", 100)
	for _, j := range jobs {
		by[j.Company] = j
	}
	return s, now, by
}

func f(v float64) *float64 { return &v }

func TestInQueueModeOnlyThePersonCanApproveAndTheClaimNeedsIt(t *testing.T) {
	// Arrange
	s, _, jobs := scanned(t, nil)
	id := jobs["Acme"].ID
	// Act + Assert: the agent alone, even with a top score, cannot approve
	if _, err := s.Update("ann", id, Patch{Status: StatusApproved, Score: f(5)}); err == nil {
		t.Fatal("approved without the person")
	}
	if _, d, _ := s.Claim("ann", id); d.Allowed {
		t.Fatal("claimed a job that was not approved")
	}
	if _, err := s.Update("ann", id, Patch{Status: StatusApproved, Score: f(4.6), UserApproved: true}); err != nil {
		t.Fatal(err)
	}
	j, d, err := s.Claim("ann", id)
	if err != nil || !d.Allowed || j.Status != StatusApplying {
		t.Fatalf("%+v %+v %v", j, d, err)
	}
	if j, err = s.Applied("ann", id, "confirmation page"); err != nil || j.Status != StatusApplied || j.AppliedAt.IsZero() {
		t.Fatalf("%+v %v", j, err)
	}
}

func TestInAutoModeAHighScoreIsEnoughAndALowOneIsNot(t *testing.T) {
	s, _, jobs := scanned(t, func(p *Profile) { p.Mode = ModeAuto })
	if _, err := s.Update("ann", jobs["Acme"].ID, Patch{Status: StatusApproved, Score: f(4.2)}); err == nil {
		t.Fatal("approved below minScore")
	}
	if _, err := s.Update("ann", jobs["Acme"].ID, Patch{Status: StatusApproved, Score: f(4.7)}); err != nil {
		t.Fatal(err)
	}
	if _, d, _ := s.Claim("ann", jobs["Acme"].ID); !d.Allowed {
		t.Fatalf("%+v", d)
	}
}

func TestTheDailyCapAndOneApplicationPerCompany(t *testing.T) {
	// Arrange: auto mode, a cap of 2
	s, now, jobs := scanned(t, func(p *Profile) { p.Mode = ModeAuto; p.DailyCap = 2 })
	for _, name := range []string{"Acme", "Globex", "Initech"} {
		if _, err := s.Update("ann", jobs[name].ID, Patch{Status: StatusApproved, Score: f(4.9)}); err != nil {
			t.Fatal(err)
		}
	}
	// Act + Assert
	for _, name := range []string{"Acme", "Globex"} {
		if _, d, _ := s.Claim("ann", jobs[name].ID); !d.Allowed {
			t.Fatalf("%s: %+v", name, d)
		}
	}
	if _, d, _ := s.Claim("ann", jobs["Initech"].ID); d.Allowed || !strings.Contains(d.Reasons[0], "daily cap") {
		t.Fatalf("%+v", d)
	}
	*now = now.Add(25 * time.Hour) // the claims expired and a day passed
	if _, d, _ := s.Claim("ann", jobs["Initech"].ID); !d.Allowed {
		t.Fatalf("%+v", d)
	}
}

func TestASecondJobAtTheSameCompanyWaitsUnlessThePersonSaysSo(t *testing.T) {
	s, _, _ := newSvc(t)
	setup(t, s, func(p *Profile) { p.Mode = ModeAuto })
	// two Acme jobs on the board
	s.Bases[Greenhouse] = s.Bases[Greenhouse]
	res, _ := s.Scan(context.Background(), "ann")
	_ = res
	jobs, _ := s.List("ann", "all", 100)
	var acme Job
	for _, j := range jobs {
		if j.Company == "Acme" {
			acme = j
		}
	}
	other := acme
	other.ID, other.Title, other.Status = "other", "Go Architect", StatusApproved
	other.Score, other.Approval = 4.8, ApprovedByAuto
	appendJob(t, s, other)
	if _, err := s.Update("ann", acme.ID, Patch{Status: StatusApproved, Score: f(4.8)}); err != nil {
		t.Fatal(err)
	}
	if _, d, _ := s.Claim("ann", acme.ID); !d.Allowed {
		t.Fatalf("%+v", d)
	}
	if _, err := s.Applied("ann", acme.ID, ""); err != nil {
		t.Fatal(err)
	}
	if _, d, _ := s.Claim("ann", "other"); d.Allowed || !strings.Contains(strings.Join(d.Reasons, ";"), "already applied to Acme") {
		t.Fatalf("%+v", d)
	}
	if _, err := s.Update("ann", "other", Patch{Status: StatusApproved, UserApproved: true}); err != nil {
		t.Fatal(err)
	}
	if _, d, _ := s.Claim("ann", "other"); !d.Allowed {
		t.Fatalf("the person approved it: %+v", d)
	}
}

func appendJob(t *testing.T, s *Service, j Job) {
	t.Helper()
	path, jobs, err := s.load("ann")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.save(path, append(jobs, j)); err != nil {
		t.Fatal(err)
	}
}

func TestAnApplicationMustBeClaimedFirstAndAClaimNobodyFinishesIsReleased(t *testing.T) {
	s, now, jobs := scanned(t, nil)
	id := jobs["Acme"].ID
	if _, err := s.Applied("ann", id, ""); err == nil {
		t.Fatal("applied without a claim")
	}
	if _, err := s.Update("ann", id, Patch{Status: StatusApproved, UserApproved: true}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Claim("ann", id); err != nil {
		t.Fatal(err)
	}
	*now = now.Add(31 * time.Minute)
	if j, _ := s.Get("ann", id); j.Status != StatusApproved {
		t.Fatalf("claim not released: %+v", j)
	}
}

func TestABlacklistedCompanyCannotBeClaimedEvenIfApprovedEarlier(t *testing.T) {
	s, _, jobs := scanned(t, nil)
	id := jobs["Acme"].ID
	if _, err := s.Update("ann", id, Patch{Status: StatusApproved, UserApproved: true}); err != nil {
		t.Fatal(err)
	}
	p, _ := s.Profile("ann")
	p.Blacklist = []string{"Acme"}
	if _, err := s.SetProfile("ann", p); err != nil {
		t.Fatal(err)
	}
	if _, d, _ := s.Claim("ann", id); d.Allowed {
		t.Fatalf("%+v", d)
	}
}

func TestProfileNormalizeAppliesDefaultsAndRefusesBadValues(t *testing.T) {
	p, err := Profile{Titles: []string{" Go  dev ", "go dev", ""}}.Normalize()
	if err != nil || len(p.Titles) != 1 || p.Mode != ModeQueue || p.MinScore != 4.5 || p.QueueScore != 4 || p.DailyCap != 10 {
		t.Fatalf("%+v %v", p, err)
	}
	for name, bad := range map[string]Profile{
		"mode":  {Mode: "yolo"},
		"score": {MinScore: 6},
		"order": {MinScore: 3, QueueScore: 4},
		"cap":   {DailyCap: 500},
		"ats":   {Companies: []Company{{ATS: "workday", Slug: "x"}}},
		"slug":  {Companies: []Company{{ATS: Lever, Slug: "../x"}}},
	} {
		if _, err := bad.Normalize(); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestDetectBoard(t *testing.T) {
	for in, want := range map[string]string{
		"https://jobs.lever.co/acme/123-456":                    "lever/acme",
		"https://job-boards.greenhouse.io/acme/jobs/9":          "greenhouse/acme",
		"https://boards.greenhouse.io/embed/job_board?for=acme": "greenhouse/acme",
		"https://jobs.ashbyhq.com/acme":                         "ashby/acme",
	} {
		ats, slug, ok := DetectBoard(in)
		if !ok || ats+"/"+slug != want {
			t.Errorf("%s: %s/%s %v", in, ats, slug, ok)
		}
	}
	if _, _, ok := DetectBoard("https://example.com/careers"); ok {
		t.Error("example.com is not a board")
	}
}

func TestOnlyWholeWordsExcludeATitle(t *testing.T) {
	if containsWord("international sales", "intern", true) || !containsWord("go intern (summer)", "intern", true) {
		t.Fatal("whole word matching")
	}
	if containsWord("algorithms engineer", "go", false) || !containsWord("software engineering lead", "engineer", false) {
		t.Fatal("word-start matching")
	}
}

func TestUsersListsOnlyThoseWhoEnabledTheSearch(t *testing.T) {
	s, _, _ := newSvc(t)
	setup(t, s, nil)
	if _, err := s.SetProfile("bob", Profile{Titles: []string{"x"}}); err != nil {
		t.Fatal(err)
	}
	if u, _ := s.Users(); len(u) != 1 || u[0] != "ann" {
		t.Fatalf("%v", u)
	}
}

func TestToolsRefuseToLoosenTheLimitsWithoutTheUsersWord(t *testing.T) {
	yes := true
	if _, err := (ProfileIn{Enabled: &yes}).apply(Profile{}); err == nil {
		t.Fatal("enabled without userConfirmed")
	}
	got, err := (ProfileIn{Enabled: &yes, UserConfirmed: true}).apply(Profile{})
	if err != nil || !got.Enabled {
		t.Fatalf("%+v %v", got, err)
	}
}

func TestAnswersAreAskedOnceKeptAndMarkedFreshUntilRead(t *testing.T) {
	svc := New(t.TempDir())
	// Act
	q, err := svc.Ask("ann", "Notice period?", "j1")
	if err != nil {
		t.Fatal(err)
	}
	again, _ := svc.Ask("ann", "  notice PERIOD ", "")
	open, fresh, _ := svc.AnswerCounts("ann")
	// Assert: the same words are the same question
	if again.ID != q.ID || open != 1 || fresh != 0 {
		t.Fatalf("%+v %d %d", again, open, fresh)
	}
	if _, err := svc.Answer("ann", q.ID, "one month"); err != nil {
		t.Fatal(err)
	}
	if open, fresh, _ = svc.AnswerCounts("ann"); open != 0 || fresh != 1 {
		t.Fatalf("%d %d", open, fresh)
	}
	if got, _ := svc.Ask("ann", "Notice period?", ""); got.Answer != "one month" {
		t.Fatalf("an asked question must return its answer: %+v", got)
	}
	if err := svc.MarkSeen("ann"); err != nil {
		t.Fatal(err)
	}
	if _, fresh, _ = svc.AnswerCounts("ann"); fresh != 0 {
		t.Fatal(fresh)
	}
	// what the person said in the chat is saved with its question, and another person sees none of it
	if _, err := svc.Remember("ann", "Salary floor", "70k EUR"); err != nil {
		t.Fatal(err)
	}
	if qs, _ := svc.Questions("ann"); len(qs) != 2 {
		t.Fatalf("%+v", qs)
	}
	if qs, _ := svc.Questions("bob"); len(qs) != 0 {
		t.Fatalf("%+v", qs)
	}
	if _, err := svc.Answer("ann", "nope", "x"); err == nil {
		t.Fatal("unknown question")
	}
	if err := svc.Forget("ann", q.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Ask("ann", "", ""); err == nil {
		t.Fatal("empty question")
	}
}
