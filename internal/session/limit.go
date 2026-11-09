package session

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/ChrAlpha/tmux-agent-experience/internal/eventlog"
)

// MaxRecordBytes bounds a session file as encoded. Readers refuse a larger
// one, so no writer publishes one.
const MaxRecordBytes = 4 << 20

// Parts of a change that LimitError names.
const (
	partRecord  = "record"
	partPending = "pending record"
	partFinal   = "final record"
	partEnded   = "ended record"
	partEvent   = "event"
)

// LimitError refuses a change one of whose publications would exceed its
// encoded limit; nothing of the change was published. It names the session
// and the part, never what the part holds.
type LimitError struct {
	Agent, ID string
	// Part is what would exceed its limit: the record a plain write
	// publishes, a change's pending or final record, an ended record, or the
	// event reporting the change.
	Part        string
	Size, Limit int
	err         error
}

func (e *LimitError) Error() string {
	where := "session"
	if validAgent(e.Agent) && ValidID(e.ID) {
		where = e.Agent + "/" + e.ID
	}
	return fmt.Sprintf("limit_exceeded: %s %s encodes to %d bytes, over %d", where, e.Part, e.Size, e.Limit)
}

// Unwrap returns the event log's refusal of an event.
func (e *LimitError) Unwrap() error { return e.err }

var errIdentity = errors.New("invalid session agent or id")

// limits are the encoded bounds a change is admitted under: its readers'.
// White-box tests lower the event bound, which the record bound otherwise
// keeps out of reach for session events.
type limits struct{ record, event int }

var readers = limits{record: MaxRecordBytes, event: eventlog.MaxRecordBytes}

// marshal returns the bytes that publish rec, refused as part when they
// exceed the record limit.
func (l limits) marshal(part string, rec Record) ([]byte, error) {
	data, err := json.Marshal(rec)
	if err != nil {
		return nil, err
	}
	if len(data) > l.record {
		return nil, &LimitError{Agent: rec.Agent, ID: rec.ID, Part: part, Size: len(data), Limit: l.record}
	}
	return data, nil
}

// encode returns the bytes that publish rec, refused as part when they
// exceed the record limit, and the record they decode to: what a change
// derives from what it publishes is derived from this, as a repair derives
// it from the record stored.
func (l limits) encode(part string, rec Record) (Record, []byte, error) {
	data, err := l.marshal(part, rec)
	if err != nil {
		return Record{}, nil, err
	}
	var out Record
	if err := json.Unmarshal(data, &out); err != nil {
		return Record{}, nil, err
	}
	return out, data, nil
}

// canonical is rec as readers would decode it. Decoding replaces invalid
// UTF-8, which can make distinct strings equal: what a change derives from a
// record (whether it is logged, its running subagents, its reports) is
// derived from this form. Decoding at most halves an encoding: only invalid
// UTF-8 changes, each byte encoded in at most six bytes and decoded to a
// replacement character encoded in three, so a record encoding to more than
// twice the record limit fits no publication and is refused undecoded.
func (l limits) canonical(rec Record) (Record, error) {
	data, err := json.Marshal(rec)
	if err != nil {
		return Record{}, err
	}
	if len(data) > 2*l.record {
		return Record{}, &LimitError{Agent: rec.Agent, ID: rec.ID, Part: partRecord, Size: len(data), Limit: l.record}
	}
	var out Record
	if err := json.Unmarshal(data, &out); err != nil {
		return Record{}, err
	}
	return out, nil
}

// admit refuses an event whose line would exceed the event limit.
func (l limits) admit(ev eventlog.Event) error {
	n, err := eventlog.Admit(ev)
	var refused *eventlog.LimitError
	switch {
	case errors.As(err, &refused):
	case err != nil:
		return err
	case n <= l.event:
		return nil
	}
	return &LimitError{Agent: ev.Agent, ID: ev.Session, Part: partEvent, Size: n, Limit: l.event, err: err}
}

// AdmitEvent refuses a session's event that the event log would refuse, as a
// *LimitError: an event of tmag's that changes no record, checked before
// what it reports is delivered.
func AdmitEvent(ev eventlog.Event) error { return readers.admit(ev) }

// Prepared is a change of a session admitted whole: every record it
// publishes is encoded within the record limit, and the event it logs
// within the event log's. Store.Publish makes it. The caller holds the
// session's lock from preparing the change until it is published.
type Prepared struct {
	agent, id string
	marked    []byte          // published first: the change, marked; nil when there is no mark
	event     *eventlog.Event // logged next; nil when the change logs nothing
	final     []byte          // published last
	end       bool            // the change ends the session, whose record is deleted last
	rec       Record          // the record final holds
}

// PrepareSave admits what Store.Save publishes for rec: when its state is
// one the event log does not report yet, rec marked with change, the event
// named change reporting it, and rec as logged.
func PrepareSave(rec Record, change string) (Prepared, error) {
	return readers.prepare(rec, eventlog.Event{Event: change}, false)
}

// PrepareReport admits what Store.Report publishes for rec and ev, a
// delivery of tmag's: rec marked with ev kept whole, ev reporting rec, and
// rec as logged.
func PrepareReport(rec Record, ev eventlog.Event) (Prepared, error) {
	return readers.prepare(rec, ev, true)
}

// PrepareEnd admits what Store.End publishes when ev (SessionEnd or Exit)
// ends the session: an ended record and the event that ends it.
func PrepareEnd(rec Record, ev eventlog.Event) (Prepared, error) {
	return readers.prepareEnd(rec, ev)
}

// prepare derives every publication from the record in canonical form, with
// every pending prompt named as Settle would name it, and from the marked
// record as stored: Settle, repairing a change its writer did not finish,
// then derives the same event and final record from the same bytes. A record
// derived from decoded strings decodes to itself, so the final record needs
// no decoding.
func (l limits) prepare(rec Record, ev eventlog.Event, delivery bool) (Prepared, error) {
	if !validAgent(rec.Agent) || !ValidID(rec.ID) {
		return Prepared{}, errIdentity
	}
	rec.Change, rec.Delivery = "", nil
	rec, err := l.canonical(rec)
	if err != nil {
		return Prepared{}, err
	}
	rec.name(time.Now())
	if !delivery && !rec.Unlogged() {
		data, err := l.marshal(partRecord, rec)
		if err != nil {
			return Prepared{}, err
		}
		return Prepared{agent: rec.Agent, id: rec.ID, final: data, rec: rec}, nil
	}
	rec.Change = ev.Event
	if delivery {
		kept := ev
		rec.Delivery = &kept
	}
	marked, data, err := l.encode(partPending, rec)
	if err != nil {
		return Prepared{}, err
	}
	p, err := l.logged(marked)
	p.marked = data
	return p, err
}

// logged admits what logging a marked record publishes: the event of the
// delivery it keeps, or else one named after its change, reporting it, and
// the record with what it reports logged and its mark cleared.
func (l limits) logged(marked Record) (Prepared, error) {
	ev := eventlog.Event{Event: marked.Change}
	switch {
	case marked.Delivery != nil && marked.Delivery.Event == marked.Change:
		ev = *marked.Delivery
	case ev.Event == "":
		ev.Event = "Unlogged"
	}
	marked.Report(&ev)
	if err := l.admit(ev); err != nil {
		return Prepared{}, err
	}
	final := marked
	final.Logged, final.Change, final.Delivery = marked.Reports(), "", nil
	data, err := l.marshal(partFinal, final)
	if err != nil {
		return Prepared{}, err
	}
	return Prepared{agent: final.Agent, id: final.ID, event: &ev, final: data, rec: final}, nil
}

// prepareEnd admits an end. The ended record keeps only what the event that
// ends the session reports, whatever else the session held: a session ends
// whatever size its record reached, and a repair logs the end as decided.
func (l limits) prepareEnd(rec Record, ev eventlog.Event) (Prepared, error) {
	if !validAgent(rec.Agent) || !ValidID(rec.ID) {
		return Prepared{}, errIdentity
	}
	if ev.Event != eventlog.End && ev.Event != eventlog.Exit {
		return Prepared{}, errors.New("a session ends with SessionEnd or Exit")
	}
	cwd := ev.Cwd
	if cwd == "" {
		cwd = rec.Cwd
	}
	ended, data, err := l.encode(partEnded, Record{Agent: rec.Agent, ID: rec.ID, Cwd: cwd, LastHookAt: ev.LastHookAt, Ended: ev.Event})
	if err != nil {
		return Prepared{}, err
	}
	end := endEvent(ended)
	if err := l.admit(end); err != nil {
		return Prepared{}, err
	}
	return Prepared{agent: ended.Agent, id: ended.ID, marked: data, event: &end, end: true}, nil
}

// endEvent is the event that ends the session an ended record marks; all of
// it comes from the record.
func endEvent(ended Record) eventlog.Event {
	return eventlog.Event{Agent: ended.Agent, Session: ended.ID, Event: ended.Ended, Cwd: ended.Cwd, LastHookAt: ended.LastHookAt}
}
