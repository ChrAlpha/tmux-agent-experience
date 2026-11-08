package cli

import (
	"context"
	"fmt"
	"time"

	"github.com/ChrAlpha/tmux-agent-experience/internal/diagnostic"
	"github.com/ChrAlpha/tmux-agent-experience/internal/eventlog"
	"github.com/ChrAlpha/tmux-agent-experience/internal/maintenance"
	"github.com/ChrAlpha/tmux-agent-experience/internal/observe"
	"github.com/ChrAlpha/tmux-agent-experience/internal/policy"
	"github.com/ChrAlpha/tmux-agent-experience/internal/session"
)

const doctorProbeBudget = 2 * time.Second
const doctorRecordLimit = 128
const doctorRecordBytes = 8 << 20
const doctorSampleLimit = 16

type OptionEvidence struct {
	Effective string `json:"effective,omitempty"`
	Name      string `json:"name"`
	State     string `json:"state"`
	Value     *int   `json:"value,omitempty"`
}
type UnconfirmedSend struct {
	Agent  string     `json:"agent"`
	ID     string     `json:"id"`
	SentAt time.Time  `json:"sent_at"`
	HookAt *time.Time `json:"hook_at,omitempty"`
	Status string     `json:"status"`
}
type RecordEvidence struct {
	Complete         bool                  `json:"complete"`
	Readable         int                   `json:"readable_in_probe"`
	KnownFaults      uint64                `json:"known_faults"`
	SampleLimit      int                   `json:"sample_limit"`
	EntryLimit       int                   `json:"entry_limit"`
	ByteLimit        int                   `json:"byte_limit"`
	Truncated        bool                  `json:"truncated"`
	Issues           []session.RecordIssue `json:"issues"`
	UnconfirmedTotal int                   `json:"unconfirmed_in_probe"`
	Unconfirmed      []UnconfirmedSend     `json:"unconfirmed_sends"`
	Note             string                `json:"note"`
}
type OperationalEvidence struct {
	At          time.Time            `json:"at"`
	Complete    bool                 `json:"complete"`
	BudgetMS    int                  `json:"budget_ms"`
	Recording   string               `json:"recording"`
	Options     []OptionEvidence     `json:"options"`
	History     diagnostic.History   `json:"historical_faults"`
	Observation observe.Diagnosis    `json:"observation"`
	Maintenance maintenance.Evidence `json:"maintenance"`
	Log         eventlog.Health      `json:"event_log"`
	Records     RecordEvidence       `json:"records"`
}

func (a *app) operationalEvidence(parent context.Context) OperationalEvidence {
	return a.probeOperational(parent, policy.Resolve)
}
func (a *app) probeOperational(parent context.Context, resolve func(context.Context, policy.Spec) (int, error)) OperationalEvidence {
	ctx, cancel := context.WithTimeout(parent, doctorProbeBudget)
	defer cancel()
	e := OperationalEvidence{At: time.Now().UTC(), Complete: true, BudgetMS: int(doctorProbeBudget / time.Millisecond), Recording: "unknown", Options: []OptionEvidence{}}
	for _, spec := range []policy.Spec{policy.Watch, policy.Notify, policy.Reconcile, policy.Observation, policy.Diagnostics} {
		n, err := resolve(ctx, spec)
		option := OptionEvidence{Name: spec.Name, State: "readable"}
		if err != nil {
			option.State = "unavailable"
			e.Complete = false
		} else {
			value := n
			option.Value = &value
			option.Effective = fmt.Sprintf("%d", n)
			if n == 0 {
				option.Effective = "off"
			} else if spec == policy.Diagnostics {
				option.Effective = "on"
			}
			if spec == policy.Diagnostics {
				e.Recording = "on"
				if n == 0 {
					e.Recording = "off"
				}
			}
		}
		e.Options = append(e.Options, option)
	}
	historyCtx, stopHistory := context.WithTimeout(ctx, 50*time.Millisecond)
	e.History = diagnostic.Probe(historyCtx, a.log.Dir, doctorSampleLimit)
	stopHistory()
	observationCtx, stopObservation := context.WithTimeout(ctx, 350*time.Millisecond)
	e.Observation = observe.Diagnose(observationCtx, a.log.Dir)
	stopObservation()
	maintenanceCtx, stopMaintenance := context.WithTimeout(ctx, 50*time.Millisecond)
	e.Maintenance = maintenance.Probe(maintenanceCtx, a.log.Dir, e.Recording)
	stopMaintenance()
	logCtx, stopLog := context.WithTimeout(ctx, 100*time.Millisecond)
	e.Log = a.log.ProbeHealth(logCtx)
	stopLog()
	recordsCtx, stopRecords := context.WithTimeout(ctx, 800*time.Millisecond)
	catalog, err := a.store().ProbeContext(recordsCtx, doctorRecordLimit, doctorRecordBytes)
	stopRecords()
	e.Records = recordEvidence(catalog, err)
	e.Complete = e.Complete && ctx.Err() == nil && e.History.State != "unreadable" && e.Observation.Complete && e.Maintenance.State != "unreadable" && e.Records.Complete && e.Log.Complete
	return e
}
func recordEvidence(c session.Catalog, err error) RecordEvidence {
	r := RecordEvidence{Complete: err == nil && c.Complete, Readable: len(c.Records), KnownFaults: c.IssueCount, SampleLimit: doctorSampleLimit, EntryLimit: doctorRecordLimit, ByteLimit: doctorRecordBytes, Issues: []session.RecordIssue{}, Unconfirmed: []UnconfirmedSend{}, Note: "bounded current probe; incomplete results never establish absence; contact time does not establish state freshness"}
	for _, i := range c.Issues {
		if len(r.Issues) < doctorSampleLimit {
			r.Issues = append(r.Issues, i)
		}
	}
	r.Truncated = !r.Complete || uint64(len(r.Issues)) < r.KnownFaults
	for _, rec := range c.Records {
		if rec.Sent.IsZero() {
			continue
		}
		r.UnconfirmedTotal++
		if len(r.Unconfirmed) < doctorSampleLimit {
			item := UnconfirmedSend{Agent: rec.Agent, ID: rec.ID, SentAt: rec.Sent, Status: "unconfirmed"}
			if rec.Observation != nil && !rec.Observation.HookAt.IsZero() {
				at := rec.Observation.HookAt
				item.HookAt = &at
			}
			r.Unconfirmed = append(r.Unconfirmed, item)
		}
	}
	if r.UnconfirmedTotal > len(r.Unconfirmed) {
		r.Truncated = true
	}
	return r
}

func operationalChecks(e OperationalEvidence) []check {
	history := check{"diagnostics", "ok", fmt.Sprintf("historical only: %d recorded occurrence(s), %d buckets, %d unbucketed; %d sample(s), truncated=%t; recording=%s; counts best effort", e.History.RecordedTotal, e.History.BucketsTotal, e.History.Overflow, len(e.History.Buckets), e.History.Truncated, e.Recording)}
	if e.History.TotalSaturated {
		history.Detail += "; total saturated (lower bound)"
	}
	if e.History.State != "readable" {
		history.Status = "warn"
		history.Detail = "history " + e.History.State + "; counts unavailable, not current success"
	}
	if e.Recording != "on" {
		history.Status = "warn"
		history.Detail += "; current recording " + e.Recording
	}
	observation := check{"observation", "ok", fmt.Sprintf("attempt=%s, lock=%s, image=%s; %s", e.Observation.LastAttempt.Outcome, e.Observation.Lock, e.Observation.Image, e.Observation.Note)}
	if s := e.Observation.LastSuccess; s != nil {
		observation.Detail += fmt.Sprintf("; last successful generation=%d", s.Generation)
		if s.At != nil {
			observation.Detail += " at " + s.At.UTC().Format(time.RFC3339Nano)
		} else {
			observation.Detail += " at unknown time"
		}
	}
	if !e.Observation.Complete || e.Observation.Image != "current" || e.Observation.LastAttempt.Outcome != "succeeded" {
		observation.Status = "warn"
	}
	if at := e.Observation.LastAttempt.At; at != nil {
		observation.Detail += "; attempt at " + at.UTC().Format(time.RFC3339Nano)
	}
	if at := e.Observation.LastAttempt.CompletedAt; at != nil {
		observation.Detail += "; completed at " + at.UTC().Format(time.RFC3339Nano)
	}
	if e.Observation.LastAttempt.FailureCode != "" {
		observation.Detail += "; last completed failure=" + e.Observation.LastAttempt.FailureCode
	}
	if e.Observation.LastAttempt.PreviousFailure != "" {
		observation.Detail += "; prior completed failure=" + e.Observation.LastAttempt.PreviousFailure
	}
	upkeep := check{"maintenance", "ok", fmt.Sprintf("recording=%s, evidence=%s, newest known pass=%s; %s", e.Recording, e.Maintenance.State, e.Maintenance.Outcome, e.Maintenance.Note)}
	if s := e.Maintenance.Status; s != nil {
		upkeep.Detail += fmt.Sprintf("; admitted=%d completed=%d", s.Admitted, s.Completed)
		if p := s.LastAdmitted; p != nil {
			upkeep.Detail += fmt.Sprintf("; last admission #%d at %s", p.ID, p.At.UTC().Format(time.RFC3339Nano))
		}
		if p := s.LastCompleted; p != nil {
			upkeep.Detail += fmt.Sprintf("; newest completed admission #%d at %s: examined=%d removed=%d deferrals=%d classes=%d failure=%s", p.ID, p.At.UTC().Format(time.RFC3339Nano), p.Work.Examined, p.Work.Removed, p.Work.Deferrals, p.Work.Classes, p.Failure)
		}
	}
	if e.Maintenance.State != "readable" || e.Maintenance.Outcome != "completed" {
		upkeep.Status = "warn"
	}
	records := check{"records", "ok", fmt.Sprintf("%d readable record(s), %d known fault(s); complete=%t, truncated=%t (limits %d entries/%d bytes)", e.Records.Readable, e.Records.KnownFaults, e.Records.Complete, e.Records.Truncated, e.Records.EntryLimit, e.Records.ByteLimit)}
	switch {
	case e.Records.KnownFaults > 0:
		records.Status = "fail"
		records.Detail += "; preserve metadata and repair storage permissions or restore a known-good record"
	case !e.Records.Complete:
		records.Status = "warn"
		records.Detail += "; remaining records unknown"
	}
	agents := check{"agents", "ok", fmt.Sprintf("%d readable sessions in bounded probe; %d unconfirmed sends; contact is independent of state freshness", e.Records.Readable, e.Records.UnconfirmedTotal)}
	if !e.Records.Complete {
		agents.Status = "warn"
		agents.Detail += "; total unknown"
	}
	options := check{"policy", "ok", "bounded realm option lookups complete; limits apply to cooperating versions"}
	for _, o := range e.Options {
		if o.State != "readable" {
			options.Status = "warn"
			options.Detail = "one or more realm option lookups unavailable; effective policy unknown"
		}
	}
	return []check{logHealthCheck(e.Log), history, observation, upkeep, records, agents, options}
}

func (a *app) printOperational(e OperationalEvidence) {
	fmt.Fprintf(a.stdout, "  probe at %s: complete=%t, shared budget=%dms\n", e.At.UTC().Format(time.RFC3339Nano), e.Complete, e.BudgetMS)
	for _, o := range e.Options {
		if o.Value != nil {
			fmt.Fprintf(a.stdout, "  %s=%s\n", o.Name, o.Effective)
		} else {
			fmt.Fprintf(a.stdout, "  %s: unavailable\n", o.Name)
		}
	}
	for _, b := range e.History.Buckets {
		ref := ""
		if b.Agent != "" {
			ref = " " + b.Agent
		}
		if b.ID != "" {
			ref += "/" + b.ID
		}
		fmt.Fprintf(a.stdout, "  historical %s/%s/%s count=%d first=%s last=%s clock_rollbacks=%d%s\n", b.Component, b.Operation, b.Category, b.Count, b.First.UTC().Format(time.RFC3339Nano), b.Last.UTC().Format(time.RFC3339Nano), b.ClockRollbacks, ref)
	}
	for _, i := range e.Records.Issues {
		fmt.Fprintf(a.stdout, "  record %s/%s: %s\n", i.Agent, i.ID, i.Category)
	}
	for _, s := range e.Records.Unconfirmed {
		fmt.Fprintf(a.stdout, "  %s/%s: send at %s unconfirmed; this does not establish failure or non-delivery\n", s.Agent, s.ID, s.SentAt.UTC().Format(time.RFC3339Nano))
	}
}
