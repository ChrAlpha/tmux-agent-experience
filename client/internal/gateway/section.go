package gateway

import "time"

// Sections a session sorts into on the page.
const (
	SectionInbox   = "inbox"
	SectionWorking = "working"
	SectionSettled = "settled"
	SectionUnknown = "unknown"
)

// Section names the inbox group a session belongs to: what waits for the
// person, what works, what is settled, and what does not report.
func Section(s Session) string { return section(s) }

// section sorts one session into the page's inbox. A turn the person has not
// seen is finished and waits; an idle session with no turn, or on a seen
// turn, is settled.
func section(s Session) string {
	switch {
	case s.State == "blocked":
		return SectionInbox
	case s.State == "idle" && s.Turn != "" && s.Turn != s.Seen:
		return SectionInbox
	case s.State == "working":
		return SectionWorking
	case s.State == "idle":
		return SectionSettled
	}
	return SectionUnknown
}

// waiting is how long the session has waited: its state change, or its
// oldest open request when blocked.
func waiting(s Session) time.Time {
	if s.State == "blocked" && len(s.Requests) > 0 {
		oldest := s.Requests[0].Since
		for _, q := range s.Requests[1:] {
			if q.Since.Before(oldest) {
				oldest = q.Since
			}
		}
		return oldest
	}
	if s.Since != nil {
		return *s.Since
	}
	return time.Time{}
}
