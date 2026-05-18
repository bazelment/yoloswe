package meetingbot

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Each side of the range is either MM:SS or HH:MM:SS, with one or more digits
// per field, so long meetings (past 99:59 or hour-qualified timestamps) are
// parsed into structured events instead of silently folding into continuation
// text and disappearing from grounding/research indexing.
var transcriptLineRE = regexp.MustCompile(`^\[(\d{1,2}:)?(\d{1,3}):(\d{2})-(\d{1,2}:)?(\d{1,3}):(\d{2})\]\s+([^:]+):\s*(.*)$`)

// LoadTranscriptFile reads a timestamped speaker transcript from disk.
func LoadTranscriptFile(path string) ([]MeetingEvent, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	events, err := ParseTranscript(f)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return events, nil
}

// ParseTranscript parses lines like "[MM:SS-MM:SS] Speaker: text". Each side of
// the range may also be hour-qualified ("[H:MM:SS-HH:MM:SS]") and the minutes
// field accepts up to three digits, so long meetings parse correctly.
// Non-matching continuation lines are appended to the prior event.
func ParseTranscript(r io.Reader) ([]MeetingEvent, error) {
	return parseTranscriptScanner(bufio.NewScanner(r))
}

func parseTranscriptScanner(scanner *bufio.Scanner) ([]MeetingEvent, error) {
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	var events []MeetingEvent
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		m := transcriptLineRE.FindStringSubmatch(line)
		if m == nil {
			if len(events) > 0 {
				i := len(events) - 1
				events[i].Text = strings.TrimSpace(events[i].Text + " " + line)
				events[i].Raw = strings.TrimSpace(events[i].Raw + "\n" + line)
			}
			continue
		}
		start, err := parseStamp(m[1], m[2], m[3])
		if err != nil {
			return nil, err
		}
		end, err := parseStamp(m[4], m[5], m[6])
		if err != nil {
			return nil, err
		}
		events = append(events, MeetingEvent{
			Start:   start,
			End:     end,
			Speaker: strings.TrimSpace(m[7]),
			Text:    strings.TrimSpace(m[8]),
			Raw:     line,
			Index:   len(events),
		})
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return events, nil
}

// parseStamp converts an optional "HH:" prefix plus minutes and seconds fields
// into a duration. hourText is "" for the MM:SS form and "HH:" for HH:MM:SS.
func parseStamp(hourText, minText, secText string) (time.Duration, error) {
	var hours int
	if hourText != "" {
		h, err := strconv.Atoi(strings.TrimSuffix(hourText, ":"))
		if err != nil {
			return 0, err
		}
		hours = h
	}
	minutes, err := strconv.Atoi(minText)
	if err != nil {
		return 0, err
	}
	seconds, err := strconv.Atoi(secText)
	if err != nil {
		return 0, err
	}
	return time.Duration(hours)*time.Hour + time.Duration(minutes)*time.Minute + time.Duration(seconds)*time.Second, nil
}

func formatEvent(e MeetingEvent) string {
	return fmt.Sprintf("[%s-%s] %s: %s", formatStamp(e.Start), formatStamp(e.End), e.Speaker, e.Text)
}

func formatStamp(d time.Duration) string {
	total := int(d.Seconds())
	if total < 0 {
		total = 0
	}
	hours := total / 3600
	minutes := (total % 3600) / 60
	seconds := total % 60
	if hours > 0 {
		// Round-trips through transcriptLineRE's optional HH: group.
		return fmt.Sprintf("%02d:%02d:%02d", hours, minutes, seconds)
	}
	return fmt.Sprintf("%02d:%02d", minutes, seconds)
}
