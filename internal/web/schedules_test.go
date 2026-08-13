// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package web

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SukramJ/go-daikin2mqtt/internal/config"
	"github.com/SukramJ/go-daikin2mqtt/internal/schedule"
)

// scheduleServer builds a server with a real engine over a temp store.
func scheduleServer(t *testing.T, groups func() map[string][]string) (*Server, *schedule.Engine) {
	t.Helper()
	store := schedule.NewStore(filepath.Join(t.TempDir(), "schedules.json"))
	eng, err := schedule.NewEngine(schedule.Options{
		Store:    store,
		Logger:   slog.New(slog.DiscardHandler),
		Timezone: "UTC",
	})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	srv := New(Deps{
		Cfg:      &config.Config{Language: "de", ScheduleEnable: true},
		Schedule: eng,
		Groups:   groups,
		Logger:   slog.New(slog.DiscardHandler),
	})
	return srv, eng
}

// do issues a request against the server and returns the recorder.
func do(t *testing.T, s *Server, method, target string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		r = httptest.NewRequest(method, target, bytes.NewReader(b))
		r.Header.Set("Content-Type", "application/json")
	} else {
		r = httptest.NewRequest(method, target, http.NoBody)
	}
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	return w
}

// newSchedule builds a valid schedule payload.
func newSchedule(name string, devices ...string) schedule.Schedule {
	targets := make([]schedule.Target, 0, len(devices))
	for _, d := range devices {
		targets = append(targets, schedule.Target{DeviceID: d})
	}
	sp := 21.5
	return schedule.Schedule{
		Name:    name,
		Enabled: true,
		Targets: targets,
		Blocks: []schedule.Block{{
			Days: []string{"mon", "tue"}, Start: "06:00", End: "08:00",
			Action: schedule.Action{Power: schedule.PowerOn, HVACMode: schedule.ModeHeat, Setpoint: &sp},
		}},
	}
}

func TestSchedulesDisabled(t *testing.T) {
	// No engine: every route answers 503 with a code the UI can translate,
	// which is a clearer signal than a 404.
	srv := New(Deps{Cfg: &config.Config{}, Logger: slog.New(slog.DiscardHandler)})
	for _, target := range []string{"/api/schedules", "/api/schedules/preview?device=x"} {
		w := do(t, srv, http.MethodGet, target, nil)
		if w.Code != http.StatusServiceUnavailable {
			t.Errorf("GET %s = %d, want 503", target, w.Code)
		}
		var e apiError
		if err := json.Unmarshal(w.Body.Bytes(), &e); err != nil || e.Code != errCodeDisabled {
			t.Errorf("GET %s body = %s, want code %s", target, w.Body, errCodeDisabled)
		}
	}
}

func TestSchedulesTypedNilEngineIsDisabled(t *testing.T) {
	// A typed nil (*schedule.Engine)(nil) would pass a plain != nil check.
	var eng *schedule.Engine
	srv := New(Deps{Cfg: &config.Config{}, Schedule: eng, Logger: slog.New(slog.DiscardHandler)})
	if w := do(t, srv, http.MethodGet, "/api/schedules", nil); w.Code != http.StatusServiceUnavailable {
		t.Errorf("GET /api/schedules = %d, want 503", w.Code)
	}
}

func TestScheduleCRUD(t *testing.T) {
	srv, eng := scheduleServer(t, nil)

	// --- create ---
	w := do(t, srv, http.MethodPost, "/api/schedules", scheduleWrite{Schedule: newSchedule("Werktag", "dev-1")})
	if w.Code != http.StatusCreated {
		t.Fatalf("POST = %d (%s), want 201", w.Code, w.Body)
	}
	var created struct {
		Revision int               `json:"revision"`
		Schedule schedule.Schedule `json:"schedule"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// The id is derived from the name, not supplied by the client.
	if created.Schedule.ID != "werktag" {
		t.Errorf("id = %q, want werktag", created.Schedule.ID)
	}
	// Blocks without an id get one, so the UI need not invent them.
	if created.Schedule.Blocks[0].ID == "" {
		t.Error("block id must be filled in")
	}

	// --- list ---
	w = do(t, srv, http.MethodGet, "/api/schedules", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("GET = %d, want 200", w.Code)
	}
	var list schedulesResponse
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(list.Schedules) != 1 || list.Schedules[0].ID != "werktag" {
		t.Fatalf("list = %+v", list.Schedules)
	}
	if list.SlotMinutes != schedule.SlotMinutes || len(list.Days) != 7 || list.Days[0] != "mon" {
		t.Errorf("editing metadata = %d / %v", list.SlotMinutes, list.Days)
	}
	if list.Timezone != "UTC" {
		t.Errorf("timezone = %q, want UTC", list.Timezone)
	}

	// --- update ---
	upd := created.Schedule
	upd.Name = "Arbeitstag"
	rev := list.Revision
	w = do(t, srv, http.MethodPut, "/api/schedules/werktag", scheduleWrite{Revision: &rev, Schedule: upd})
	if w.Code != http.StatusOK {
		t.Fatalf("PUT = %d (%s), want 200", w.Code, w.Body)
	}
	got := eng.Document()
	if got.Schedules[0].Name != "Arbeitstag" {
		t.Errorf("name = %q, want Arbeitstag", got.Schedules[0].Name)
	}
	// Renaming must not move the id: it is the HA entity identity.
	if got.Schedules[0].ID != "werktag" {
		t.Errorf("id = %q, want werktag (frozen)", got.Schedules[0].ID)
	}

	// --- enable toggle ---
	w = do(t, srv, http.MethodPost, "/api/schedules/werktag/enable", enableRequest{Enabled: false})
	if w.Code != http.StatusOK {
		t.Fatalf("enable = %d (%s), want 200", w.Code, w.Body)
	}
	if eng.Document().Schedules[0].Enabled {
		t.Error("schedule must be disabled")
	}

	// --- delete ---
	w = do(t, srv, http.MethodDelete, "/api/schedules/werktag", nil)
	if w.Code != http.StatusNoContent {
		t.Fatalf("DELETE = %d, want 204", w.Code)
	}
	if len(eng.Document().Schedules) != 0 {
		t.Error("schedule must be gone")
	}
}

func TestScheduleSlugCollision(t *testing.T) {
	srv, _ := scheduleServer(t, nil)
	for range 2 {
		if w := do(t, srv, http.MethodPost, "/api/schedules", scheduleWrite{Schedule: newSchedule("Werktag", "dev-1")}); w.Code != http.StatusCreated {
			t.Fatalf("POST = %d (%s)", w.Code, w.Body)
		}
	}
	w := do(t, srv, http.MethodGet, "/api/schedules", nil)
	var list schedulesResponse
	_ = json.Unmarshal(w.Body.Bytes(), &list)
	if len(list.Schedules) != 2 {
		t.Fatalf("schedules = %d, want 2", len(list.Schedules))
	}
	if list.Schedules[1].ID != "werktag-2" {
		t.Errorf("second id = %q, want werktag-2", list.Schedules[1].ID)
	}
}

func TestScheduleRevisionConflict(t *testing.T) {
	srv, _ := scheduleServer(t, nil)
	w := do(t, srv, http.MethodPost, "/api/schedules", scheduleWrite{Schedule: newSchedule("Werktag", "dev-1")})
	if w.Code != http.StatusCreated {
		t.Fatalf("POST = %d (%s)", w.Code, w.Body)
	}

	stale := 0 // the revision before the create
	upd := newSchedule("Werktag", "dev-1")
	upd.ID = "werktag"
	w = do(t, srv, http.MethodPut, "/api/schedules/werktag", scheduleWrite{Revision: &stale, Schedule: upd})
	if w.Code != http.StatusConflict {
		t.Fatalf("PUT with a stale revision = %d, want 409", w.Code)
	}
	var e apiError
	_ = json.Unmarshal(w.Body.Bytes(), &e)
	if e.Code != errCodeRevision {
		t.Errorf("code = %q, want %q", e.Code, errCodeRevision)
	}
}

func TestScheduleValidationError(t *testing.T) {
	srv, _ := scheduleServer(t, nil)
	bad := newSchedule("Kaputt", "dev-1")
	bad.Blocks[0].Start = "06:20" // off the 30-minute grid

	w := do(t, srv, http.MethodPost, "/api/schedules", scheduleWrite{Schedule: bad})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("POST = %d, want 400", w.Code)
	}
	var e apiError
	_ = json.Unmarshal(w.Body.Bytes(), &e)
	if e.Code != errCodeValidation {
		t.Errorf("code = %q, want %q", e.Code, errCodeValidation)
	}
	if !strings.Contains(e.Error, "30-minute") {
		t.Errorf("error = %q, want it to name the problem", e.Error)
	}
}

func TestScheduleRequiresName(t *testing.T) {
	srv, _ := scheduleServer(t, nil)
	w := do(t, srv, http.MethodPost, "/api/schedules", scheduleWrite{Schedule: newSchedule("  ", "dev-1")})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("POST = %d, want 400", w.Code)
	}
	var e apiError
	_ = json.Unmarshal(w.Body.Bytes(), &e)
	if e.Field != "name" {
		t.Errorf("field = %q, want name", e.Field)
	}
}

func TestScheduleContentTypeRequired(t *testing.T) {
	srv, _ := scheduleServer(t, nil)
	// A cross-site form can only send simple content types; requiring JSON
	// blocks them without a token.
	r := httptest.NewRequest(http.MethodPost, "/api/schedules", strings.NewReader("{}"))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusUnsupportedMediaType {
		t.Errorf("POST without JSON content type = %d, want 415", w.Code)
	}
}

func TestScheduleBadJSON(t *testing.T) {
	srv, _ := scheduleServer(t, nil)
	r := httptest.NewRequest(http.MethodPost, "/api/schedules", strings.NewReader("{"))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("POST = %d, want 400", w.Code)
	}
	var e apiError
	_ = json.Unmarshal(w.Body.Bytes(), &e)
	if e.Code != errCodeBadJSON {
		t.Errorf("code = %q, want %q", e.Code, errCodeBadJSON)
	}
}

func TestScheduleNotFound(t *testing.T) {
	srv, _ := scheduleServer(t, nil)
	if w := do(t, srv, http.MethodDelete, "/api/schedules/nope", nil); w.Code != http.StatusNotFound {
		t.Errorf("DELETE unknown = %d, want 404", w.Code)
	}
	w := do(t, srv, http.MethodPut, "/api/schedules/nope", scheduleWrite{Schedule: newSchedule("X", "dev-1")})
	if w.Code != http.StatusNotFound {
		t.Errorf("PUT unknown = %d, want 404", w.Code)
	}
}

func TestSchedulePreview(t *testing.T) {
	srv, _ := scheduleServer(t, nil)
	if w := do(t, srv, http.MethodPost, "/api/schedules", scheduleWrite{Schedule: newSchedule("Werktag", "dev-1")}); w.Code != http.StatusCreated {
		t.Fatalf("POST = %d (%s)", w.Code, w.Body)
	}

	w := do(t, srv, http.MethodGet, "/api/schedules/preview?device=dev-1", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("preview = %d (%s), want 200", w.Code, w.Body)
	}
	var prev previewResponse
	if err := json.Unmarshal(w.Body.Bytes(), &prev); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// Two days × one block.
	if len(prev.Segments) != 2 {
		t.Fatalf("segments = %d, want 2: %+v", len(prev.Segments), prev.Segments)
	}
	seg := prev.Segments[0]
	if seg.Day != 0 || seg.From != "06:00" || seg.To != "08:00" || seg.HVACMode != "heat" {
		t.Errorf("segment = %+v", seg)
	}
	if seg.Setpoint == nil || *seg.Setpoint != 21.5 {
		t.Errorf("setpoint = %v", seg.Setpoint)
	}
	// Two blocks per week, each with a start and an end.
	if prev.Counts["switches_per_week"] != 4 {
		t.Errorf("switch points = %d, want 4", prev.Counts["switches_per_week"])
	}

	// An untargeted device resolves to an empty week rather than an error.
	w = do(t, srv, http.MethodGet, "/api/schedules/preview?device=other", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("preview (other) = %d", w.Code)
	}
	_ = json.Unmarshal(w.Body.Bytes(), &prev)
	if len(prev.Segments) != 0 {
		t.Errorf("segments for an untargeted device = %d, want 0", len(prev.Segments))
	}
}

func TestSchedulePreviewRequiresDevice(t *testing.T) {
	srv, _ := scheduleServer(t, nil)
	w := do(t, srv, http.MethodGet, "/api/schedules/preview", nil)
	if w.Code != http.StatusBadRequest {
		t.Errorf("preview without device = %d, want 400", w.Code)
	}
}

func TestSchedulePreviewConflicts(t *testing.T) {
	groups := func() map[string][]string {
		return map[string][]string{"0J723746": {"dev-bed", "dev-living"}}
	}
	srv, _ := scheduleServer(t, groups)

	heat := newSchedule("Heizen", "dev-living")
	heat.Blocks[0].Start = "22:00"
	heat.Blocks[0].End = "06:00"
	if w := do(t, srv, http.MethodPost, "/api/schedules", scheduleWrite{Schedule: heat}); w.Code != http.StatusCreated {
		t.Fatalf("POST heat = %d (%s)", w.Code, w.Body)
	}
	cool := newSchedule("Kuehlen", "dev-bed")
	cool.Blocks[0].Start = "23:00"
	cool.Blocks[0].End = "04:00"
	cool.Blocks[0].Action.HVACMode = schedule.ModeCool
	if w := do(t, srv, http.MethodPost, "/api/schedules", scheduleWrite{Schedule: cool}); w.Code != http.StatusCreated {
		t.Fatalf("POST cool = %d (%s)", w.Code, w.Body)
	}

	w := do(t, srv, http.MethodGet, "/api/schedules/preview?device=dev-bed", nil)
	var prev previewResponse
	_ = json.Unmarshal(w.Body.Bytes(), &prev)
	if len(prev.Conflicts) == 0 {
		t.Fatal("want a mode conflict between the two devices of one outdoor unit")
	}
	c := prev.Conflicts[0]
	if c.Group != "0J723746" || len(c.Heating) != 1 || len(c.Cooling) != 1 {
		t.Errorf("conflict = %+v", c)
	}
	if c.From != "23:00" {
		t.Errorf("conflict starts at %q, want 23:00", c.From)
	}

	// Without a group source there is nothing to compare against.
	srvNoGroups, _ := scheduleServer(t, nil)
	w = do(t, srvNoGroups, http.MethodGet, "/api/schedules/preview?device=dev-bed", nil)
	_ = json.Unmarshal(w.Body.Bytes(), &prev)
	if len(prev.Conflicts) != 0 {
		t.Errorf("conflicts without groups = %d, want 0", len(prev.Conflicts))
	}
}

func TestScheduleModeOptionsAreLocalized(t *testing.T) {
	srv, _ := scheduleServer(t, nil)
	// Without a catalog the raw values are served, so the editor still works.
	got := srv.modeOptions()
	if len(got) != 5 || got[0].Value != "heat" {
		t.Fatalf("modes = %+v", got)
	}
	if got[0].Label != "heat" {
		t.Errorf("label without a catalog = %q, want the raw value", got[0].Label)
	}
}

func TestNormalizeBlockIDs(t *testing.T) {
	s := schedule.Schedule{Blocks: []schedule.Block{
		{ID: "b1"}, {}, {ID: "b1"}, {},
	}}
	normalizeBlockIDs(&s)
	seen := map[string]bool{}
	for i, b := range s.Blocks {
		if b.ID == "" {
			t.Errorf("block %d has no id", i)
		}
		if seen[b.ID] {
			t.Errorf("duplicate block id %q", b.ID)
		}
		seen[b.ID] = true
	}
}

func boolPtr(b bool) *bool { return &b }

// newOutdoorSchedule builds a valid outdoor schedule payload.
func newOutdoorSchedule(name, serial string) schedule.Schedule {
	demand := 70.0
	return schedule.Schedule{
		Name:    name,
		Type:    schedule.TypeOutdoor,
		Enabled: true,
		Targets: []schedule.Target{{OutdoorSerial: serial}},
		Blocks: []schedule.Block{{
			Days: []string{"mon", "tue"}, Start: "22:00", End: "06:00",
			Action: schedule.Action{OutdoorSilent: boolPtr(true), Demand: &demand},
		}},
	}
}

func TestSchedulesListOffersOutdoorUnits(t *testing.T) {
	groups := func() map[string][]string {
		return map[string][]string{"0J723746": {"dev-b", "dev-a"}}
	}
	srv, _ := scheduleServer(t, groups)

	w := do(t, srv, http.MethodGet, "/api/schedules", nil)
	var list schedulesResponse
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(list.Outdoor) != 1 {
		t.Fatalf("outdoor units = %+v, want 1", list.Outdoor)
	}
	u := list.Outdoor[0]
	if u.Serial != "0J723746" || u.Key != "outdoor:0J723746" {
		t.Errorf("unit = %+v", u)
	}
	// Members are sorted so the UI order is stable across polls.
	if len(u.Members) != 2 || u.Members[0] != "dev-a" {
		t.Errorf("members = %v, want them sorted", u.Members)
	}

	// Without a group source there is nothing to offer — but the field is an
	// empty array rather than null, so the UI can iterate unconditionally.
	srvNoGroups, _ := scheduleServer(t, nil)
	w = do(t, srvNoGroups, http.MethodGet, "/api/schedules", nil)
	_ = json.Unmarshal(w.Body.Bytes(), &list)
	if list.Outdoor == nil || len(list.Outdoor) != 0 {
		t.Errorf("outdoor units without groups = %+v, want an empty array", list.Outdoor)
	}
}

func TestOutdoorScheduleCRUD(t *testing.T) {
	groups := func() map[string][]string {
		return map[string][]string{"0J723746": {"dev-a", "dev-b"}}
	}
	srv, eng := scheduleServer(t, groups)

	w := do(t, srv, http.MethodPost, "/api/schedules",
		scheduleWrite{Schedule: newOutdoorSchedule("Nachtruhe", "0J723746")})
	if w.Code != http.StatusCreated {
		t.Fatalf("POST = %d (%s), want 201", w.Code, w.Body)
	}
	got := eng.Document().Schedules[0]
	if got.Kind() != schedule.TypeOutdoor {
		t.Errorf("type = %q, want outdoor", got.Type)
	}
	if got.Targets[0].OutdoorSerial != "0J723746" {
		t.Errorf("target = %+v", got.Targets[0])
	}
	if got.Blocks[0].Action.OutdoorSilent == nil || !*got.Blocks[0].Action.OutdoorSilent {
		t.Errorf("action = %+v", got.Blocks[0].Action)
	}

	// The preview resolves it under the outdoor key.
	w = do(t, srv, http.MethodGet, "/api/schedules/preview?target=outdoor:0J723746", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("preview = %d (%s)", w.Code, w.Body)
	}
	var prev previewResponse
	if err := json.Unmarshal(w.Body.Bytes(), &prev); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(prev.Segments) == 0 {
		t.Fatal("preview has no segments")
	}
	seg := prev.Segments[0]
	if seg.OutdoorSilent == nil || !*seg.OutdoorSilent {
		t.Errorf("segment carries no outdoor_silent: %+v", seg)
	}
	if seg.Demand == nil || *seg.Demand != 70 {
		t.Errorf("segment demand = %v, want 70", seg.Demand)
	}
	// An outdoor target cannot have a heat/cool conflict with itself.
	if len(prev.Conflicts) != 0 {
		t.Errorf("outdoor preview must report no mode conflicts, got %+v", prev.Conflicts)
	}
}

func TestOutdoorScheduleRejectsIndoorFields(t *testing.T) {
	srv, _ := scheduleServer(t, nil)
	bad := newOutdoorSchedule("Kaputt", "0J723746")
	bad.Blocks[0].Action.Power = schedule.PowerOn
	bad.Blocks[0].Action.HVACMode = schedule.ModeHeat

	w := do(t, srv, http.MethodPost, "/api/schedules", scheduleWrite{Schedule: bad})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("POST = %d, want 400", w.Code)
	}
	var e apiError
	_ = json.Unmarshal(w.Body.Bytes(), &e)
	if e.Code != errCodeValidation || !strings.Contains(e.Error, "indoor schedule") {
		t.Errorf("error = %+v, want a type mismatch", e)
	}
}

func TestPreviewAcceptsLegacyDeviceParam(t *testing.T) {
	srv, _ := scheduleServer(t, nil)
	if w := do(t, srv, http.MethodPost, "/api/schedules",
		scheduleWrite{Schedule: newSchedule("Werktag", "dev-1")}); w.Code != http.StatusCreated {
		t.Fatalf("POST = %d (%s)", w.Code, w.Body)
	}
	// An SPA cached from 0.9.x still sends ?device=.
	w := do(t, srv, http.MethodGet, "/api/schedules/preview?device=dev-1", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("preview with ?device = %d", w.Code)
	}
	var prev previewResponse
	_ = json.Unmarshal(w.Body.Bytes(), &prev)
	if len(prev.Segments) != 2 {
		t.Errorf("segments = %d, want 2", len(prev.Segments))
	}
}
