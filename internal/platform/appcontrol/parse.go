package appcontrol

import (
	"encoding/xml"
	"strings"
	"time"
)

// The XML reading lives here, untagged, rather than beside the wevtutil call
// in collect_windows.go. The subprocess is the only part that needs Windows;
// turning its output into Records is a pure function over captured event data,
// and putting the seam here is what lets it be table-tested on the Linux
// runner where `go test ./...` actually runs (CLAUDE.md, Test discipline).

// eventLogXML is the subset of the Windows event schema this reads. Every
// <Data> node is kept with its Name attribute: the field set differs between
// Windows builds, so the reader takes what is there and assumes nothing is
// present (docs/knowledges/20260904/0300-the-reputation-verdict-is-in-event-3118.md).
type eventLogXML struct {
	Events []struct {
		System struct {
			EventID     int `xml:"EventID"`
			TimeCreated struct {
				SystemTime string `xml:"SystemTime,attr"`
			} `xml:"TimeCreated"`
		} `xml:"System"`
		Data []struct {
			Name  string `xml:"Name,attr"`
			Value string `xml:",chardata"`
		} `xml:"EventData>Data"`
	} `xml:"Event"`
}

// parseRecords turns wevtutil's output into the records Explain groups.
// Records older than cutoff, and records about files that are not ours, are
// dropped. ok is false when the output could not be parsed at all — which the
// caller must not report as "nothing was refused".
func parseRecords(out []byte, cutoff time.Time) (records []Record, ok bool) {
	// wevtutil emits a bare sequence of <Event> elements, which is not a
	// document. Wrap it so encoding/xml will parse it.
	var parsed eventLogXML
	if err := xml.Unmarshal([]byte("<Events>"+string(out)+"</Events>"), &parsed); err != nil {
		return nil, false
	}

	records = make([]Record, 0, len(parsed.Events))
	for _, e := range parsed.Events {
		when := parseSystemTime(e.System.TimeCreated.SystemTime)
		if !when.IsZero() && !cutoff.IsZero() && when.Before(cutoff) {
			continue
		}
		fields := make(map[string]string, len(e.Data))
		var paths []string
		for _, d := range e.Data {
			v := strings.TrimSpace(d.Value)
			if d.Name != "" {
				fields[d.Name] = v
			}
			if looksLikeNTPath(v) {
				paths = append(paths, v)
			}
		}
		// Named fields first; the positional fallback covers a host where the
		// provider manifest is not registered and wevtutil emits bare <Data>
		// elements. In that layout the blocked file is named before the
		// process that asked for it.
		blocked := fields["FileNameBuffer"]
		requester := fields["ProcessNameBuffer"]
		if blocked == "" && len(paths) > 0 {
			blocked = paths[0]
			if requester == "" && len(paths) > 1 {
				requester = paths[1]
			}
		}
		program := MatchProgram(blocked)
		if program == "" {
			continue
		}
		records = append(records, Record{
			ID:        e.System.EventID,
			When:      when,
			Program:   program,
			Requester: baseName(requester),
			Fields:    fields,
		})
	}
	return records, true
}

// looksLikeNTPath is the one unambiguous shape in a positional EventData
// array: a kernel device path.
func looksLikeNTPath(s string) bool {
	return strings.HasPrefix(s, `\Device\`) || strings.HasPrefix(s, `\??\`)
}

// baseName reduces a device path to the file name. The full path of the
// process that tried to start ours is Event Viewer's business; the reader of a
// toast wants to know it was the shell, not which volume it lives on.
func baseName(p string) string {
	p = strings.ReplaceAll(p, "/", `\`)
	if i := strings.LastIndex(p, `\`); i >= 0 {
		return p[i+1:]
	}
	return p
}

// parseSystemTime reads the event schema's timestamp. An unparseable one
// yields the zero time, which Explain treats as "no timestamp" rather than as
// the epoch.
func parseSystemTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if t, err := time.Parse(layout, s); err == nil {
			return t
		}
	}
	return time.Time{}
}
