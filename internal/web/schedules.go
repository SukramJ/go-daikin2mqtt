// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package web

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/SukramJ/go-daikin2mqtt/internal/schedule"
)

// Error codes the SPA resolves through its i18n bundle (sched.err.<code>).
// The `error` string stays English and developer-facing, as everywhere else in
// this API; the code is what the UI translates.
const (
	errCodeBadJSON     = "invalid_json"
	errCodeValidation  = "validation_failed"
	errCodeRevision    = "revision_conflict"
	errCodeNotFound    = "not_found"
	errCodeDisabled    = "scheduler_disabled"
	errCodeBadRequest  = "bad_request"
	errCodeStoreFailed = "store_failed"
)

// schedulesResponse is the payload of GET /api/schedules: everything the
// calendar needs except the device list (which comes from /api/devices).
type schedulesResponse struct {
	Revision  int                 `json:"revision"`
	Timezone  string              `json:"timezone"`
	Schedules []schedule.Schedule `json:"schedules"`
	// Modes are the selectable HVAC modes with labels already localized from
	// the catalog's operation_mode entry, so the UI needs no second translation
	// table for words it already shows elsewhere.
	Modes []modeOption `json:"modes"`
	// Outdoor lists the outdoor units an outdoor schedule can target. Only the
	// coordinator learns them from the poll, so an installation whose groups are
	// not yet known simply offers none.
	Outdoor []outdoorUnitView `json:"outdoor_units"`
	// SlotMinutes is the editing grid, so the UI does not hard-code it.
	SlotMinutes int `json:"slot_minutes"`
	// Days are the weekday keys in storage order (Monday first, in every
	// language); the UI renders their names via Intl.
	Days []string `json:"days"`
}

// modeOption pairs an hvac mode value with its localized label.
type modeOption struct {
	Value string `json:"value"`
	Label string `json:"label"`
}

// outdoorUnitView is one schedulable outdoor unit: its serial (the target
// identity) and the devices sharing it, which the UI shows so the operator can
// tell one unit from another.
type outdoorUnitView struct {
	Serial  string   `json:"serial"`
	Key     string   `json:"key"`
	Members []string `json:"members"`
}

// outdoorUnits lists the outdoor groups, sorted for a stable UI order.
func (s *Server) outdoorUnits() []outdoorUnitView {
	out := []outdoorUnitView{}
	if s.groups == nil {
		return out
	}
	for serial, members := range s.groups() {
		sorted := append([]string(nil), members...)
		sort.Strings(sorted)
		out = append(out, outdoorUnitView{
			Serial:  serial,
			Key:     schedule.OutdoorKey(serial),
			Members: sorted,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Serial < out[j].Serial })
	return out
}

// hvacToONECTA maps the scheduler's HA mode names to the ONECTA values the
// catalog carries labels for, so the editor shows exactly the words the
// operation_mode select entity uses.
var hvacToONECTA = map[schedule.HVACMode]string{
	schedule.ModeHeat:    "heating",
	schedule.ModeCool:    "cooling",
	schedule.ModeAuto:    "auto",
	schedule.ModeDry:     "dry",
	schedule.ModeFanOnly: "fanOnly",
}

// modeOrder fixes the order the modes are offered in.
var modeOrder = []schedule.HVACMode{
	schedule.ModeHeat, schedule.ModeCool, schedule.ModeAuto,
	schedule.ModeDry, schedule.ModeFanOnly,
}

// modeOptions builds the localized mode list from the catalog.
func (s *Server) modeOptions() []modeOption {
	lang := s.cfg.Language
	if lang == "" {
		lang = "en"
	}
	out := make([]modeOption, 0, len(modeOrder))
	for _, m := range modeOrder {
		// Fall back to the raw value when the catalog is absent (tests) or has
		// no operation_mode entry, so the editor always has something to show.
		label := string(m)
		if s.catalog != nil {
			if e, ok := s.catalog.ByTopic("operation_mode"); ok {
				label = e.LocalizedLabel(hvacToONECTA[m], lang)
			}
		}
		out = append(out, modeOption{Value: string(m), Label: label})
	}
	return out
}

// requireScheduler answers with 503 when the scheduler is disabled, so the UI
// can tell "off" from "broken".
func (s *Server) requireScheduler(w http.ResponseWriter) bool {
	if s.schedule == nil {
		writeAPIError(w, http.StatusServiceUnavailable, errCodeDisabled,
			"the scheduler is disabled (set SCHEDULE_ENABLE)", "")
		return false
	}
	return true
}

// handleSchedulesList returns every schedule plus the editing metadata.
func (s *Server) handleSchedulesList(w http.ResponseWriter, _ *http.Request) {
	if !s.requireScheduler(w) {
		return
	}
	doc := s.schedule.Document()
	tz := doc.Timezone
	if tz == "" {
		tz = s.schedule.Location().String()
	}
	writeJSON(w, http.StatusOK, schedulesResponse{
		Revision:    doc.Revision,
		Timezone:    tz,
		Schedules:   doc.Schedules,
		Modes:       s.modeOptions(),
		Outdoor:     s.outdoorUnits(),
		SlotMinutes: schedule.SlotMinutes,
		Days:        weekdayKeys(),
	})
}

// weekdayKeys returns the storage weekday keys, Monday first.
func weekdayKeys() []string {
	out := make([]string, 7)
	for i := range out {
		out[i] = schedule.DayKey(i)
	}
	return out
}

// scheduleWrite is the body of POST/PUT: the schedule plus the revision the
// editor loaded, so a concurrent change is caught instead of overwritten.
type scheduleWrite struct {
	Revision *int              `json:"revision"`
	Schedule schedule.Schedule `json:"schedule"`
}

// handleScheduleCreate adds a schedule, deriving its id from the name.
func (s *Server) handleScheduleCreate(w http.ResponseWriter, r *http.Request) {
	if !s.requireScheduler(w) || !requireJSON(w, r) {
		return
	}
	var req scheduleWrite
	if !decodeJSON(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.Schedule.Name) == "" {
		writeAPIError(w, http.StatusBadRequest, errCodeValidation, "name is required", "name")
		return
	}

	doc := s.schedule.Document()
	taken := make(map[string]bool, len(doc.Schedules))
	for i := range doc.Schedules {
		taken[doc.Schedules[i].ID] = true
	}
	// The id is derived once, here, and then frozen: renaming later changes
	// only the display name, so the HA entity id survives the rename.
	req.Schedule.ID = schedule.UniqueSlug(req.Schedule.Name, taken)
	normalizeBlockIDs(&req.Schedule)
	doc.Schedules = append(doc.Schedules, req.Schedule)

	out, err := s.schedule.Replace(doc, revisionOf(req.Revision))
	if err != nil {
		s.writeScheduleError(w, err)
		return
	}
	s.log.Info("web.schedule_created",
		slog.String("schedule", req.Schedule.ID), slog.Int("revision", out.Revision))
	created, _ := out.Find(req.Schedule.ID)
	writeJSON(w, http.StatusCreated, map[string]any{
		"revision": out.Revision,
		"schedule": created,
	})
}

// handleScheduleUpdate replaces one schedule wholesale.
func (s *Server) handleScheduleUpdate(w http.ResponseWriter, r *http.Request) {
	if !s.requireScheduler(w) || !requireJSON(w, r) {
		return
	}
	id := r.PathValue("id")
	var req scheduleWrite
	if !decodeJSON(w, r, &req) {
		return
	}

	doc := s.schedule.Document()
	idx := -1
	for i := range doc.Schedules {
		if doc.Schedules[i].ID == id {
			idx = i
			break
		}
	}
	if idx < 0 {
		writeAPIError(w, http.StatusNotFound, errCodeNotFound, "no such schedule: "+id, "")
		return
	}
	// The id is not editable: it is the entity identity in Home Assistant.
	req.Schedule.ID = id
	normalizeBlockIDs(&req.Schedule)
	doc.Schedules[idx] = req.Schedule

	out, err := s.schedule.Replace(doc, revisionOf(req.Revision))
	if err != nil {
		s.writeScheduleError(w, err)
		return
	}
	s.log.Info("web.schedule_updated", slog.String("schedule", id), slog.Int("revision", out.Revision))
	updated, _ := out.Find(id)
	writeJSON(w, http.StatusOK, map[string]any{"revision": out.Revision, "schedule": updated})
}

// handleScheduleDelete removes a schedule.
func (s *Server) handleScheduleDelete(w http.ResponseWriter, r *http.Request) {
	if !s.requireScheduler(w) {
		return
	}
	id := r.PathValue("id")
	if err := s.schedule.Delete(id); err != nil {
		s.writeScheduleError(w, err)
		return
	}
	s.log.Info("web.schedule_deleted", slog.String("schedule", id))
	w.WriteHeader(http.StatusNoContent)
}

// enableRequest is the body of the enable toggle.
type enableRequest struct {
	Enabled bool `json:"enabled"`
}

// handleScheduleEnable flips one schedule's switch — the same path the Home
// Assistant switch takes through the coordinator.
func (s *Server) handleScheduleEnable(w http.ResponseWriter, r *http.Request) {
	if !s.requireScheduler(w) || !requireJSON(w, r) {
		return
	}
	var req enableRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	id := r.PathValue("id")
	if err := s.schedule.SetEnabled(id, req.Enabled); err != nil {
		s.writeScheduleError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"revision": s.schedule.Document().Revision,
		"enabled":  req.Enabled,
	})
}

// previewResponse is the resolved week for one device — the single source of
// truth the calendar renders from, so the browser never re-implements the
// priority rules.
type previewResponse struct {
	// DeviceID carries the resolved target key (a device id, or
	// "outdoor:<serial>"). The field name is kept for compatibility.
	DeviceID   string         `json:"device_id"`
	Segments   []segmentView  `json:"segments"`
	Active     *claimView     `json:"active"`
	NextChange string         `json:"next_change,omitempty"`
	Conflicts  []conflictView `json:"conflicts"`
	Counts     map[string]int `json:"counts"`
}

// segmentView is one effective block of the calendar, bounded within a day.
type segmentView struct {
	Day           int      `json:"day"`
	From          string   `json:"from"`
	To            string   `json:"to"`
	Schedule      string   `json:"schedule_id"`
	Name          string   `json:"schedule_name"`
	Block         string   `json:"block_id"`
	Label         string   `json:"label,omitempty"`
	Priority      int      `json:"priority"`
	Power         string   `json:"power,omitempty"`
	HVACMode      string   `json:"hvac_mode,omitempty"`
	Setpoint      *float64 `json:"setpoint,omitempty"`
	OutdoorSilent *bool    `json:"outdoor_silent,omitempty"`
	Econo         *bool    `json:"econo,omitempty"`
	Demand        *float64 `json:"demand,omitempty"`
}

// claimView is the block in force right now.
type claimView struct {
	Schedule      string   `json:"schedule_id"`
	Name          string   `json:"schedule_name"`
	Block         string   `json:"block_id"`
	Label         string   `json:"label,omitempty"`
	Power         string   `json:"power,omitempty"`
	HVACMode      string   `json:"hvac_mode,omitempty"`
	Setpoint      *float64 `json:"setpoint,omitempty"`
	OutdoorSilent *bool    `json:"outdoor_silent,omitempty"`
	Econo         *bool    `json:"econo,omitempty"`
	Demand        *float64 `json:"demand,omitempty"`
}

// conflictView is a window where one outdoor unit is asked to heat and cool at
// once. Device ids stay ids; the UI resolves them to names it already has.
type conflictView struct {
	Group   string   `json:"group"`
	From    string   `json:"from"`
	To      string   `json:"to"`
	FromDay int      `json:"from_day"`
	ToDay   int      `json:"to_day"`
	Heating []string `json:"heating"`
	Cooling []string `json:"cooling"`
}

// handleSchedulePreview resolves the week for one device.
func (s *Server) handleSchedulePreview(w http.ResponseWriter, r *http.Request) {
	if !s.requireScheduler(w) {
		return
	}
	// "target" is the current name; "device" stays accepted so an older cached
	// SPA keeps working after an upgrade.
	targetKey := r.URL.Query().Get("target")
	if targetKey == "" {
		targetKey = r.URL.Query().Get("device")
	}
	if targetKey == "" {
		writeAPIError(w, http.StatusBadRequest, errCodeBadRequest, "query parameter 'target' is required", "target")
		return
	}

	doc := s.schedule.Document()
	loc := s.schedule.Location()
	week := schedule.Resolve(doc, targetKey)
	now := time.Now().In(loc)
	cur := schedule.SlotAt(now)

	resp := previewResponse{
		DeviceID:  targetKey,
		Segments:  []segmentView{},
		Conflicts: []conflictView{},
		Counts:    map[string]int{},
	}
	for day := range 7 {
		for _, seg := range week.Segments(day) {
			resp.Segments = append(resp.Segments, segmentViewOf(seg))
		}
	}
	if c := week.At(cur); c != nil {
		resp.Active = &claimView{
			Schedule: c.ScheduleID, Name: c.ScheduleName, Block: c.BlockID, Label: c.Label,
			Power: string(c.Action.Power), HVACMode: string(c.Action.HVACMode), Setpoint: c.Action.Setpoint,
			OutdoorSilent: c.Action.OutdoorSilent, Econo: c.Action.Econo, Demand: c.Action.Demand,
		}
	}
	if next, _, ok := week.NextChange(cur); ok {
		resp.NextChange = schedule.SlotStart(next, now, loc).Format(time.RFC3339)
	}
	resp.Counts["switches_per_week"] = countSwitches(week)
	// A mode conflict is an indoor notion: it needs two units that could run in
	// opposite directions. An outdoor target has no such pair.
	if _, isOutdoor := schedule.OutdoorSerialOf(targetKey); !isOutdoor {
		resp.Conflicts = s.conflictsFor(doc, targetKey)
	}

	writeJSON(w, http.StatusOK, resp)
}

// segmentViewOf renders one resolved segment.
func segmentViewOf(seg schedule.Segment) segmentView {
	return segmentView{
		Day:           seg.Day,
		From:          schedule.FormatClock(seg.FromMinute),
		To:            schedule.FormatClock(seg.ToMinute),
		Schedule:      seg.Claim.ScheduleID,
		Name:          seg.Claim.ScheduleName,
		Block:         seg.Claim.BlockID,
		Label:         seg.Claim.Label,
		Priority:      seg.Claim.Priority,
		Power:         string(seg.Claim.Action.Power),
		HVACMode:      string(seg.Claim.Action.HVACMode),
		Setpoint:      seg.Claim.Action.Setpoint,
		OutdoorSilent: seg.Claim.Action.OutdoorSilent,
		Econo:         seg.Claim.Action.Econo,
		Demand:        seg.Claim.Action.Demand,
	}
}

// countSwitches counts the switch points in a resolved week — what the UI
// shows as the daily write load.
func countSwitches(week *schedule.Week) int {
	n := 0
	for slot := range schedule.SlotsPerWeek {
		prev := (slot + schedule.SlotsPerWeek - 1) % schedule.SlotsPerWeek
		if week.At(slot).Key() != week.At(prev).Key() {
			n++
		}
	}
	return n
}

// conflictsFor reports the mode conflicts of every outdoor group the device
// belongs to. Without a group source (the coordinator supplies it) there is
// nothing to check: a conflict is only meaningful between devices sharing one
// outdoor unit.
func (s *Server) conflictsFor(doc *schedule.Document, deviceID string) []conflictView {
	out := []conflictView{}
	if s.groups == nil {
		return out
	}
	for group, members := range s.groups() {
		if !contains(members, deviceID) {
			continue
		}
		sorted := append([]string(nil), members...)
		sort.Strings(sorted)
		for _, c := range schedule.Conflicts(doc, group, sorted) {
			out = append(out, conflictView{
				Group:   c.Group,
				From:    schedule.FormatClock((c.FromSlot % schedule.SlotsPerDay) * schedule.SlotMinutes),
				To:      schedule.FormatClock((c.ToSlot % schedule.SlotsPerDay) * schedule.SlotMinutes),
				FromDay: c.FromSlot / schedule.SlotsPerDay,
				ToDay:   (c.ToSlot % schedule.SlotsPerWeek) / schedule.SlotsPerDay,
				Heating: c.Heating,
				Cooling: c.Cooling,
			})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].FromDay != out[j].FromDay {
			return out[i].FromDay < out[j].FromDay
		}
		return out[i].From < out[j].From
	})
	return out
}

func contains(haystack []string, needle string) bool {
	for _, h := range haystack {
		if h == needle {
			return true
		}
	}
	return false
}

// normalizeBlockIDs fills in missing block ids so the UI can post a new block
// without inventing one.
func normalizeBlockIDs(s *schedule.Schedule) {
	taken := map[string]bool{}
	for i := range s.Blocks {
		if id := s.Blocks[i].ID; id != "" && !taken[id] {
			taken[id] = true
			continue
		}
		s.Blocks[i].ID = nextBlockID(taken)
		taken[s.Blocks[i].ID] = true
	}
}

// nextBlockID returns the lowest free "b<n>" id.
func nextBlockID(taken map[string]bool) string {
	for n := 1; ; n++ {
		id := "b" + strconv.Itoa(n)
		if !taken[id] {
			return id
		}
	}
}

// revisionOf turns the optional revision into the engine's convention
// (negative = skip the check).
func revisionOf(rev *int) int {
	if rev == nil {
		return -1
	}
	return *rev
}

// writeScheduleError maps an engine error to the right status and code.
func (s *Server) writeScheduleError(w http.ResponseWriter, err error) {
	var ve *schedule.ValidationError
	switch {
	case errors.Is(err, schedule.ErrStaleRevision):
		writeAPIError(w, http.StatusConflict, errCodeRevision, err.Error(), "")
	case errors.Is(err, schedule.ErrNotFound):
		writeAPIError(w, http.StatusNotFound, errCodeNotFound, err.Error(), "")
	case errors.As(err, &ve):
		writeAPIError(w, http.StatusBadRequest, errCodeValidation, err.Error(), "")
	default:
		s.log.Warn("web.schedule_store_failed", slog.String("err", err.Error()))
		writeAPIError(w, http.StatusInternalServerError, errCodeStoreFailed, err.Error(), "")
	}
}

// requireJSON enforces the JSON content type. Cross-site forms can only send
// "simple" content types, so this blocks them without a token — the same
// hardening handlePatch uses.
func requireJSON(w http.ResponseWriter, r *http.Request) bool {
	if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		writeAPIError(w, http.StatusUnsupportedMediaType, errCodeBadRequest,
			"Content-Type must be application/json", "")
		return false
	}
	return true
}

// decodeJSON decodes a bounded request body.
func decodeJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 256*1024)).Decode(v); err != nil {
		writeAPIError(w, http.StatusBadRequest, errCodeBadJSON, "invalid JSON body: "+err.Error(), "")
		return false
	}
	return true
}

// apiError is the error envelope. `error` is the English developer-facing
// message; `code` is the stable key the UI translates.
type apiError struct {
	Error string `json:"error"`
	Code  string `json:"code"`
	Field string `json:"field,omitempty"`
}

func writeAPIError(w http.ResponseWriter, status int, code, msg, field string) {
	writeJSON(w, status, apiError{Error: msg, Code: code, Field: field})
}
