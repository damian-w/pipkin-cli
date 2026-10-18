package pipkin

import (
	"fmt"
	"io"
	"math"
	"slices"
	"strings"
)

const outputWidth = 79

// writeOutputLine keeps terminal text ASCII and bounded, including continuation
// lines. The prefix is repeated so wrapped notes keep their indentation.
func writeOutputLine(w io.Writer, prefix, text string) error {
	prefix = outputASCII(prefix)
	if len(prefix) >= outputWidth {
		prefix = prefix[:outputWidth-1]
	}
	text = strings.TrimSpace(outputASCII(text))
	width := outputWidth - len(prefix)
	for {
		cut := len(text)
		if cut > width {
			cut = strings.LastIndexByte(text[:width+1], ' ')
			if cut <= 0 {
				cut = width
			}
		}
		line := prefix + strings.TrimRight(text[:cut], " ") + "\n"
		n, err := io.WriteString(w, line)
		if err != nil {
			return err
		}
		if n != len(line) {
			return io.ErrShortWrite
		}
		text = strings.TrimLeft(text[cut:], " ")
		if text == "" {
			return nil
		}
	}
}

func outputASCII(value string) string {
	return strings.Map(func(r rune) rune {
		if r >= ' ' && r <= '~' {
			return r
		}
		return '?'
	}, value)
}

type providerOutput struct {
	w   io.Writer
	err error
}

func (out *providerOutput) line(prefix, text string) {
	if out.err == nil {
		out.err = writeOutputLine(out.w, prefix, text)
	}
}

func (out *providerOutput) row(label, value string, width int) {
	label = strings.TrimSpace(outputASCII(label))
	if len(label) > width {
		out.line("  ", label)
		out.line("    ", value)
		return
	}
	out.line("  ", fmt.Sprintf("%-*s  %s", width, label, value))
}

// writeProviders presents the same allowance windows in cached status and fresh
// usage. It never changes readings, their source times, or the JSON representation.
func writeProviders(w io.Writer, s Status, now int64, detailed bool) error {
	out := providerOutput{w: w}
	for index, provider := range []struct{ key, name string }{{"codex", "Codex"}, {"claude", "Claude"}} {
		if index > 0 {
			out.line("", "")
		}
		reading := s.Readings[provider.key]
		message := outputProviderMessage(provider.name, s.providerError(provider.key))
		state := s.States[provider.key]
		heading := provider.name
		if detailed && reading != nil && strings.TrimSpace(reading.Plan) != "" && state != "signed_out" {
			heading += " (" + strings.TrimSpace(outputASCII(reading.Plan)) + ")"
		}
		out.line("", heading)
		if state == "signed_out" {
			out.line("  ", "Not signed in")
			if !outputCalmSignOut(message) {
				out.line("  ! ", message)
			}
			continue
		}
		failed := message != "" || state != "" && state != "available"
		if !outputHasReading(reading) {
			if failed {
				out.line("  ", "No reading available")
			} else {
				out.line("  ", "No reading yet")
			}
			if message != "" {
				out.line("  ! ", message)
			}
			continue
		}
		if failed {
			note := "Last known reading"
			if reading.Observed <= 0 || reading.Observed > now {
				note += " (age unknown)"
			} else if now-reading.Observed >= 5*60 {
				note += " (" + outputDuration(now-reading.Observed) + " ago)"
			}
			out.line("  ", note)
		}
		labels := []string{"Session", "Weekly"}
		models := make([]string, 0, len(reading.Models))
		if detailed {
			for model := range reading.Models {
				models = append(models, model)
			}
			slices.Sort(models)
			for _, model := range models {
				labels = append(labels, outputModelLabel(model))
			}
			if reading.Credits != nil {
				labels = append(labels, "Credits")
			}
			if reading.ExtraUsage != nil {
				labels = append(labels, "Extra usage")
			}
			if reading.ReportsBanked || reading.Banked != nil {
				labels = append(labels, "Banked resets")
			}
		}
		width := 0
		for _, label := range labels {
			width = max(width, min(len(outputASCII(label)), 24))
		}
		session := outputWindow(reading.Session, now)
		if reading.SessionNoCap {
			session = "No session cap"
		}
		out.row("Session", session, width)
		out.row("Weekly", outputWindow(reading.Weekly, now), width)
		if detailed {
			for _, model := range models {
				out.row(outputModelLabel(model), outputWindow(reading.Models[model], now), width)
			}
			if reading.Credits != nil {
				value := "Unknown"
				if reading.Credits.Unlimited {
					value = "Unlimited"
				} else if outputAmountKnown(reading.Credits.Balance) {
					value = fmt.Sprintf("%.2f", *reading.Credits.Balance)
				}
				out.row("Credits", value, width)
			}
			if reading.ExtraUsage != nil {
				out.row("Extra usage", outputExtraUsage(reading.ExtraUsage), width)
			}
			if reading.ReportsBanked || reading.Banked != nil {
				value := "Unknown"
				if reading.Banked != nil && *reading.Banked >= 0 {
					value = fmt.Sprint(*reading.Banked)
				}
				out.row("Banked resets", value, width)
			}
		}
		if !failed && reading.Observed > 0 && reading.Observed <= now && now-reading.Observed >= 5*60 {
			out.line("  ", "Last reading "+outputDuration(now-reading.Observed)+" ago")
		}
		if message != "" {
			out.line("  ! ", message)
		}
	}
	return out.err
}

func outputCalmSignOut(message string) bool {
	message = strings.ToLower(strings.TrimSpace(message))
	message = strings.TrimSuffix(message, ".")
	switch message {
	case "", "not signed in", "no existing sign-in found", "codex keyring sign-in is absent", "codex app sign-in is absent", "no readable codex app sign-in found":
		return true
	}
	return false
}

func outputProviderMessage(provider, message string) string {
	message = strings.TrimSpace(outputASCII(message))
	for strings.HasPrefix(message, provider+":") {
		message = strings.TrimSpace(strings.TrimPrefix(message, provider+":"))
	}
	// Old cached snapshots wrapped the sentinel into otherwise actionable text.
	// Keep the remediation while removing the contradictory trailing explanation.
	return strings.TrimSuffix(message, ": no existing sign-in found")
}

func outputHasReading(reading *Reading) bool {
	return reading != nil && (reading.SessionNoCap || reading.Session != nil || reading.Weekly != nil ||
		len(reading.Models) > 0 || reading.Credits != nil || reading.ExtraUsage != nil || reading.ReportsBanked || reading.Banked != nil)
}

func outputWindow(window *Window, now int64) string {
	if window == nil {
		return "Unknown"
	}
	if window.NotStarted {
		return "Starts with your first message"
	}
	if window.Used < 0 || window.Used > 1000 {
		return "Unknown"
	}
	text := fmt.Sprintf("%.0f%% left", float64(1000-window.Used)/10)
	if window.Reset != 0 {
		if window.Reset <= now {
			text += "  awaiting refresh"
		} else {
			text += "  resets in " + outputDuration(window.Reset-now)
		}
	}
	return text
}

func outputDuration(seconds int64) string {
	if seconds <= 0 {
		return "0s"
	}
	var major, minor int64
	var majorUnit, minorUnit string
	switch {
	case seconds >= 86400:
		major, minor, majorUnit, minorUnit = seconds/86400, seconds%86400/3600, "d", "h"
	case seconds >= 3600:
		major, minor, majorUnit, minorUnit = seconds/3600, seconds%3600/60, "h", "m"
	case seconds >= 60:
		return fmt.Sprint(seconds/60) + "m"
	default:
		return fmt.Sprint(seconds) + "s"
	}
	text := fmt.Sprint(major) + majorUnit
	if minor > 0 {
		text += " " + fmt.Sprint(minor) + minorUnit
	}
	return text
}

func outputModelLabel(model string) string {
	model = strings.TrimSpace(outputASCII(model))
	period := "weekly"
	for _, suffix := range []string{"_session", "_weekly"} {
		if strings.HasSuffix(model, suffix) {
			model = strings.TrimSuffix(model, suffix)
			period = strings.TrimPrefix(suffix, "_")
			break
		}
	}
	words := strings.Fields(strings.NewReplacer("_", " ", "-", " ").Replace(model))
	for index, word := range words {
		switch strings.ToLower(word) {
		case "gpt":
			words[index] = "GPT"
		default:
			if word[0] >= 'a' && word[0] <= 'z' {
				words[index] = strings.ToUpper(word[:1]) + word[1:]
			}
		}
	}
	if len(words) == 0 {
		return "Model " + period
	}
	return strings.Join(words, " ") + " " + period
}

func outputAmountKnown(amount *float64) bool {
	return amount != nil && !math.IsNaN(*amount) && !math.IsInf(*amount, 0) && *amount >= 0
}

func outputExtraUsage(extra *ExtraUsage) string {
	if !extra.Enabled {
		return "Off"
	}
	currency := strings.TrimSpace(outputASCII(extra.Currency))
	if currency != "" {
		currency = " " + currency
	}
	switch {
	case outputAmountKnown(extra.Used) && outputAmountKnown(extra.Limit):
		return fmt.Sprintf("%.2f of %.2f%s used", *extra.Used, *extra.Limit, currency)
	case outputAmountKnown(extra.Used):
		return fmt.Sprintf("%.2f%s used; limit unknown", *extra.Used, currency)
	case outputAmountKnown(extra.Limit):
		return fmt.Sprintf("Spend unknown; limit %.2f%s", *extra.Limit, currency)
	default:
		return "On; spend unknown"
	}
}
