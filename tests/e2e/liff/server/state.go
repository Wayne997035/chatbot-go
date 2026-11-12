package main

import (
	"chatbot-go/internal/httputil"
	"chatbot-go/internal/region"
	"chatbot-go/internal/subscription"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

// [F303-11] State of the fake API: call log, one-shot delays, persistent failures and the account/alert data the page reads back.

const (
	alertQuake   = "earthquake_tsunami"
	alertWeather = "weather_warning"
)

// goldenFiles are the shared contract fixtures; the Go contract tests in internal/liff compare handler output against the same files.
var goldenFiles = []string{
	"account", "account_empty", "alert_item", "alerts", "alerts_weather_only", "day", "month", "profile", "scale", "summary",
}

type golden map[string][]byte

// loadGolden reads every fixture byte for byte; a missing file is an error so a renamed fixture cannot go unnoticed.
func loadGolden(dir string) (golden, error) {
	g := golden{}
	for _, name := range goldenFiles {
		b, err := os.ReadFile(filepath.Join(dir, name+".json"))
		if err != nil {
			return nil, fmt.Errorf("read golden %s: %w", name, err)
		}
		g[name] = b
	}
	return g, nil
}

type failRule struct {
	Prefix string `json:"prefix"`
	Method string `json:"method"`
	Status int    `json:"status"`
}

type delayRule struct {
	Prefix string `json:"prefix"`
	Method string `json:"method"`
	MS     int    `json:"ms"`
	Late   bool   `json:"late"`
}

type callRecord struct {
	Seq    int    `json:"seq"`
	Method string `json:"method"`
	Path   string `json:"path"`
	Query  string `json:"query"`
	Status int    `json:"status"`
}

type alertItem struct {
	AlertType string   `json:"alertType"`
	Regions   []string `json:"regions"`
}

type alertsView struct {
	Subscriptions []alertItem `json:"subscriptions"`
}

type profileView struct {
	WeightKG float64 `json:"weightKg"`
	HeightCM float64 `json:"heightCm"`
	Age      int     `json:"age"`
	Sex      string  `json:"sex"`
}

type countyView struct {
	Name      string   `json:"name"`
	Townships []string `json:"townships"`
}

type monthDay struct {
	Date     string  `json:"date"`
	Intake   float64 `json:"intake"`
	Meals    int     `json:"meals"`
	Burned   float64 `json:"burned"`
	Workouts int     `json:"workouts"`
	RunKM    float64 `json:"runKm"`
	Grade    string  `json:"grade"`
}

type monthView struct {
	Month string     `json:"month"`
	Base  float64    `json:"base"`
	Days  []monthDay `json:"days"`
}

type reply struct {
	status int
	body   []byte
}

type request struct {
	method  string
	path    string
	query   string
	body    []byte
	arrival int
}

type seed struct {
	quake   bool
	weather []string
	profile profileView
	month   monthView
}

func parseSeed(g golden) (seed, error) {
	var sd seed
	var av alertsView
	if err := json.Unmarshal(g["alerts"], &av); err != nil {
		return sd, fmt.Errorf("parse alerts golden: %w", err)
	}
	for _, it := range av.Subscriptions {
		switch it.AlertType {
		case alertQuake:
			sd.quake = true
		case alertWeather:
			sd.weather = it.Regions
		}
	}
	if err := json.Unmarshal(g["profile"], &sd.profile); err != nil {
		return sd, fmt.Errorf("parse profile golden: %w", err)
	}
	if err := json.Unmarshal(g["month"], &sd.month); err != nil {
		return sd, fmt.Errorf("parse month golden: %w", err)
	}
	return sd, nil
}

type state struct {
	g       golden
	sd      seed
	regions []byte

	mu        sync.Mutex
	seq       int
	calls     []callRecord
	fails     []failRule
	delays    []delayRule
	held      int
	released  chan struct{}
	deleted   bool
	deletedAt int
	quake     bool
	weather   []string
	profile   profileView
}

func newState(g golden) (*state, error) {
	sd, err := parseSeed(g)
	if err != nil {
		return nil, err
	}
	view := struct {
		Counties []countyView `json:"counties"`
	}{Counties: make([]countyView, 0, len(region.Counties))}
	for _, name := range region.Counties {
		towns := region.Townships(name)
		if towns == nil {
			towns = []string{}
		}
		view.Counties = append(view.Counties, countyView{Name: name, Townships: towns})
	}
	regions, err := json.Marshal(view)
	if err != nil {
		return nil, fmt.Errorf("marshal regions: %w", err)
	}
	s := &state{g: g, sd: sd, regions: regions}
	s.reset()
	return s, nil
}

// reset puts the state back to the golden data and frees every held request.
func (s *state) reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.released != nil {
		close(s.released)
	}
	s.released = make(chan struct{})
	s.seq = 0
	s.calls = nil
	s.fails = nil
	s.delays = nil
	s.held = 0
	s.deleted = false
	s.deletedAt = 0
	s.quake = s.sd.quake
	s.weather = slices.Clone(s.sd.weather)
	s.profile = s.sd.profile
}

// clearRules drops every failure rule and every delay rule that has not been used yet; requests already held keep waiting.
func (s *state) clearRules() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fails = nil
	s.delays = nil
}

// release lets every held request finish now.
func (s *state) release() {
	s.mu.Lock()
	defer s.mu.Unlock()
	close(s.released)
	s.released = make(chan struct{})
}

type ruleMatch struct {
	seq   int
	index int
	fail  *failRule
	delay *delayRule
}

func ruleApplies(prefix, ruleMethod, path, method string) bool {
	return strings.HasPrefix(path, prefix) && (ruleMethod == "" || ruleMethod == method)
}

// begin logs the call, finds the failure rule (kept) and the delay rule (consumed: one shot).
func (s *state) begin(method, path, query string) ruleMatch {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seq++
	m := ruleMatch{seq: s.seq, index: len(s.calls)}
	s.calls = append(s.calls, callRecord{Seq: s.seq, Method: method, Path: path, Query: query})
	for i := range s.fails {
		if ruleApplies(s.fails[i].Prefix, s.fails[i].Method, path, method) {
			f := s.fails[i]
			m.fail = &f
			break
		}
	}
	for i := range s.delays {
		if ruleApplies(s.delays[i].Prefix, s.delays[i].Method, path, method) {
			d := s.delays[i]
			m.delay = &d
			s.delays = append(s.delays[:i], s.delays[i+1:]...)
			break
		}
	}
	return m
}

func (s *state) finish(index, status int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if index < len(s.calls) {
		s.calls[index].Status = status
	}
}

// hold keeps one request waiting until release, the delay runs out or the client goes away.
func (s *state) hold(ctx context.Context, ms int) {
	s.mu.Lock()
	s.held++
	ch := s.released
	s.mu.Unlock()
	t := time.NewTimer(time.Duration(ms) * time.Millisecond)
	defer t.Stop()
	select {
	case <-ch:
	case <-t.C:
	case <-ctx.Done():
	}
	s.mu.Lock()
	s.held--
	s.mu.Unlock()
}

// compute builds the reply (and applies the write); an injected failure replaces the real answer.
func (s *state) compute(r *request, fail *failRule) reply {
	s.mu.Lock()
	defer s.mu.Unlock()
	if fail != nil {
		code := httputil.ErrorCodeServerError
		if fail.Status == http.StatusGone {
			code = httputil.ErrorCodeDataErased
		}
		return errReply(fail.Status, code, "injected failure")
	}
	return s.route(r)
}

func (s *state) erasedSince(arrival int) bool {
	return s.deletedAt > arrival
}

func rawReply(body []byte) reply {
	return reply{status: http.StatusOK, body: body}
}

func jsonReply(v any) reply {
	b, err := json.Marshal(v)
	if err != nil {
		return errReply(http.StatusInternalServerError, httputil.ErrorCodeServerError, "marshal failed")
	}
	return reply{status: http.StatusOK, body: b}
}

func errReply(status, code int, msg string) reply {
	b, err := json.Marshal(httputil.ErrorWithCode(code, msg))
	if err != nil {
		return reply{status: status, body: []byte("{}")}
	}
	return reply{status: status, body: b}
}

func badRequest(msg string) reply {
	return errReply(http.StatusBadRequest, httputil.ErrorCodeInvalidParameter, msg)
}

func erasedReply() reply {
	return errReply(http.StatusGone, httputil.ErrorCodeDataErased, "Data was erased, nothing saved")
}

type handlerFunc func(s *state, r *request) reply

func goldenHandler(name string) handlerFunc {
	return func(s *state, _ *request) reply { return rawReply(s.g[name]) }
}

var exactRoutes = map[string]handlerFunc{
	"POST /account/sync": (*state).account,
	"GET /account":       (*state).account,
	"DELETE /account":    (*state).deleteAccount,
	"GET /alerts":        (*state).listAlerts,
	"GET /regions":       func(s *state, _ *request) reply { return rawReply(s.regions) },
	"GET /summary":       goldenHandler("summary"),
	"GET /day":           goldenHandler("day"),
	"GET /month":         (*state).monthReply,
	"GET /profile":       func(s *state, _ *request) reply { return jsonReply(s.profile) },
	"PUT /profile":       (*state).putProfile,
}

// route dispatches one request; the caller holds s.mu.
func (s *state) route(r *request) reply {
	if h, ok := exactRoutes[r.method+" "+r.path]; ok {
		return h(s, r)
	}
	switch {
	case strings.HasPrefix(r.path, "/alerts/") && r.method == http.MethodPut:
		return s.putAlert(r)
	case strings.HasPrefix(r.path, "/alerts/") && r.method == http.MethodDelete:
		return s.deleteAlert(r)
	case strings.HasPrefix(r.path, "/meals/") && r.method == http.MethodPatch:
		return rawReply(s.g["scale"])
	case (strings.HasPrefix(r.path, "/meals/") || strings.HasPrefix(r.path, "/workouts/")) && r.method == http.MethodDelete:
		return reply{status: http.StatusNoContent}
	}
	return errReply(http.StatusNotFound, httputil.ErrorCodeNotFound, "Resource not found")
}

func (s *state) account(_ *request) reply {
	if s.deleted {
		return rawReply(s.g["account_empty"])
	}
	return rawReply(s.g["account"])
}

// deleteAccount erases everything the page can read back; writes that arrived before this call now answer 410.
func (s *state) deleteAccount(r *request) reply {
	s.deleted = true
	s.deletedAt = r.arrival
	s.quake = false
	s.weather = nil
	s.profile = profileView{}
	return reply{status: http.StatusNoContent}
}

func (s *state) listAlerts(_ *request) reply {
	view := alertsView{Subscriptions: []alertItem{}}
	if s.weather != nil {
		view.Subscriptions = append(view.Subscriptions, alertItem{AlertType: alertWeather, Regions: s.weather})
	}
	if s.quake {
		view.Subscriptions = append(view.Subscriptions, alertItem{AlertType: alertQuake, Regions: []string{}})
	}
	return jsonReply(view)
}

func (s *state) putProfile(r *request) reply {
	var in profileView
	if err := json.Unmarshal(r.body, &in); err != nil {
		return badRequest("invalid body")
	}
	if s.erasedSince(r.arrival) {
		return erasedReply()
	}
	s.profile = in
	return jsonReply(in)
}

// putAlert follows PutAlert in internal/liff: regions go through the product's own NormalizeRegions.
func (s *state) putAlert(r *request) reply {
	kind := strings.TrimPrefix(r.path, "/alerts/")
	if kind != alertQuake && kind != alertWeather {
		return badRequest("invalid alert type")
	}
	var in struct {
		Regions []string `json:"regions"`
	}
	if strings.TrimSpace(string(r.body)) != "" {
		if err := json.Unmarshal(r.body, &in); err != nil {
			return badRequest("invalid body")
		}
	}
	if s.erasedSince(r.arrival) {
		return erasedReply()
	}
	if kind == alertQuake {
		s.quake = true
		return jsonReply(alertItem{AlertType: alertQuake, Regions: []string{}})
	}
	stored, err := subscription.NormalizeRegions(in.Regions)
	if err != nil {
		return badRequest("invalid regions")
	}
	s.weather = stored
	return jsonReply(alertItem{AlertType: alertWeather, Regions: stored})
}

func (s *state) deleteAlert(r *request) reply {
	switch strings.TrimPrefix(r.path, "/alerts/") {
	case alertQuake:
		s.quake = false
	case alertWeather:
		s.weather = nil
	default:
		return badRequest("invalid alert type")
	}
	return reply{status: http.StatusNoContent}
}

// monthReply returns the golden month for the golden month label and an all-zero month for any other valid label.
func (s *state) monthReply(r *request) reply {
	q, err := url.ParseQuery(r.query)
	if err != nil {
		return badRequest("invalid query")
	}
	label := q.Get("month")
	if label == s.sd.month.Month {
		return rawReply(s.g["month"])
	}
	first, err := time.Parse("2006-01", label)
	if err != nil {
		return badRequest("invalid month")
	}
	view := monthView{Month: label, Base: s.sd.month.Base, Days: []monthDay{}}
	for d := first; d.Month() == first.Month(); d = d.AddDate(0, 0, 1) {
		view.Days = append(view.Days, monthDay{Date: d.Format("2006-01-02")})
	}
	return jsonReply(view)
}

// snapshot is the GET /__e2e/state payload.
type snapshot struct {
	Calls   []callRecord `json:"calls"`
	Held    int          `json:"held"`
	Deleted bool         `json:"deleted"`
	Quake   bool         `json:"quake"`
	Weather []string     `json:"weather"`
}

func (s *state) snapshot() snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	calls := append([]callRecord{}, s.calls...)
	return snapshot{Calls: calls, Held: s.held, Deleted: s.deleted, Quake: s.quake, Weather: s.weather}
}

func (s *state) setAlerts(quake bool, weather []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.quake = quake
	s.weather = weather
}

func (s *state) addFail(f failRule) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fails = append(s.fails, f)
}

func (s *state) addDelay(d delayRule) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.delays = append(s.delays, d)
}
