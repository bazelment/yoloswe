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

var transcriptLineRE = regexp.MustCompile(`^\[(\d{2}):(\d{2})-(\d{2}):(\d{2})\]\s+([^:]+):\s*(.*)$`)

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

// ParseTranscript parses lines like "[00:02-00:05] Speaker: text". Non-matching
// continuation lines are appended to the prior event.
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
		start, err := parseStamp(m[1], m[2])
		if err != nil {
			return nil, err
		}
		end, err := parseStamp(m[3], m[4])
		if err != nil {
			return nil, err
		}
		events = append(events, MeetingEvent{
			Start:   start,
			End:     end,
			Speaker: strings.TrimSpace(m[5]),
			Text:    strings.TrimSpace(m[6]),
			Raw:     line,
			Index:   len(events),
		})
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return events, nil
}

func parseStamp(minText, secText string) (time.Duration, error) {
	minutes, err := strconv.Atoi(minText)
	if err != nil {
		return 0, err
	}
	seconds, err := strconv.Atoi(secText)
	if err != nil {
		return 0, err
	}
	return time.Duration(minutes)*time.Minute + time.Duration(seconds)*time.Second, nil
}

func formatEvent(e MeetingEvent) string {
	return fmt.Sprintf("[%s-%s] %s: %s", formatStamp(e.Start), formatStamp(e.End), e.Speaker, e.Text)
}

func formatStamp(d time.Duration) string {
	total := int(d.Seconds())
	return fmt.Sprintf("%02d:%02d", total/60, total%60)
}
