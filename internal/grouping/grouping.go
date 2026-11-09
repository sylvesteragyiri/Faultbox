// Package grouping decides which events belong to the same issue (PRD §4.3).
//
// A fingerprint is a SHA-256 over:
//
//  1. the exception class,
//  2. the normalized message, with dynamic values such as IDs, UUIDs, emails,
//     timestamps and SQL bindings replaced by placeholders, and
//  3. the top in-app stack frames, as class::function or a normalized file
//     path. Line numbers are left out so that unrelated edits that shift code
//     don't split an issue in two.
//
// Queue failures add the job class on top, so the same exception in two
// different jobs makes two issues.
package grouping

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strconv"
	"strings"
)

// Frame is one stack frame, most recent call first.
type Frame struct {
	File     string `json:"file,omitempty"`
	Line     int    `json:"line,omitempty"`
	Function string `json:"function,omitempty"`
	Class    string `json:"class,omitempty"`
	InApp    *bool  `json:"in_app,omitempty"`
}

// fingerprintFrames is how many in-app frames feed the fingerprint.
const fingerprintFrames = 5

// maxNormalizedLen bounds the message part of the fingerprint.
const maxNormalizedLen = 1000

var (
	// Laravel's QueryException appends the SQL with its bindings substituted,
	// e.g. "(Connection: mysql, SQL: select * from `users` where `id` = 5)".
	sqlSuffixRe = regexp.MustCompile(`\((?:Connection: [^,()]+, )?SQL: (.*)\)\s*$`)
	sqlCutRe    = regexp.MustCompile(`(?i)\s(values|where|set|limit|offset|in)\b`)

	uuidRe     = regexp.MustCompile(`(?i)\b[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\b`)
	emailRe    = regexp.MustCompile(`(?i)\b[a-z0-9._%+\-]+@[a-z0-9.\-]+\.[a-z]{2,}\b`)
	isoTimeRe  = regexp.MustCompile(`\b\d{4}-\d{2}-\d{2}(?:[T ]\d{2}:\d{2}(?::\d{2}(?:\.\d+)?)?(?:Z|[+\-]\d{2}:?\d{2})?)?\b`)
	ipv4Re     = regexp.MustCompile(`\b(?:\d{1,3}\.){3}\d{1,3}\b`)
	hexRe      = regexp.MustCompile(`(?i)\b(?:0x[0-9a-f]+|[0-9a-f]*\d[0-9a-f]*[a-f][0-9a-f]*|[0-9a-f]*[a-f][0-9a-f]*\d[0-9a-f]*)\b`)
	singleQRe  = regexp.MustCompile(`'[^'\n]{0,200}'`)
	numberRe   = regexp.MustCompile(`\b\d+(?:\.\d+)?\b`) // whole tokens only, so codes like HY000 survive
	spaceRe    = regexp.MustCompile(`\s+`)
	vendorPath = regexp.MustCompile(`(^|/)vendor/`)
	// Common Laravel project roots, used to drop deploy-specific prefixes
	// such as /var/www/releases/20261109/ from file paths.
	projectRootRe = regexp.MustCompile(`(^|/)(app|src|routes|database|resources|config|bootstrap|modules|packages|tests|vendor)/`)
)

// NormalizeMessage replaces dynamic values in an exception message with
// placeholders, so "User 42 not found" and "User 7 not found" group together.
func NormalizeMessage(msg string) string {
	if m := sqlSuffixRe.FindStringSubmatchIndex(msg); m != nil {
		sql := msg[m[2]:m[3]]
		if loc := sqlCutRe.FindStringIndex(sql); loc != nil {
			sql = sql[:loc[0]]
		}
		msg = msg[:m[0]] + "(SQL: " + strings.TrimSpace(sql) + " …)"
	}
	msg = uuidRe.ReplaceAllString(msg, "<uuid>")
	msg = emailRe.ReplaceAllString(msg, "<email>")
	msg = isoTimeRe.ReplaceAllString(msg, "<date>")
	msg = ipv4Re.ReplaceAllString(msg, "<ip>")
	msg = singleQRe.ReplaceAllString(msg, "'<s>'")
	msg = replaceLongHex(msg)
	msg = numberRe.ReplaceAllString(msg, "<n>")
	msg = strings.TrimSpace(spaceRe.ReplaceAllString(msg, " "))
	if len(msg) > maxNormalizedLen {
		msg = msg[:maxNormalizedLen]
	}
	return msg
}

// replaceLongHex replaces hex tokens of 8+ characters (hashes, tokens, object
// IDs) and 0x literals. Shorter tokens are left alone so words like "cafe"
// and "bad" survive.
func replaceLongHex(s string) string {
	return hexRe.ReplaceAllStringFunc(s, func(tok string) string {
		if strings.HasPrefix(strings.ToLower(tok), "0x") || len(tok) >= 8 {
			return "<hex>"
		}
		return tok
	})
}

// IsInApp reports whether a frame belongs to the application rather than a
// dependency. An explicit in_app flag from the SDK wins; otherwise frames
// under vendor/ are treated as dependencies.
func IsInApp(f Frame) bool {
	if f.InApp != nil {
		return *f.InApp
	}
	if f.File == "" {
		return false
	}
	file := strings.ReplaceAll(f.File, "\\", "/")
	return !vendorPath.MatchString(file)
}

// NormalizePath strips deploy-specific prefixes, leaving the path from a
// known project root: /var/www/releases/42/app/Models/User.php → app/Models/User.php.
func NormalizePath(file string) string {
	file = strings.ReplaceAll(file, "\\", "/")
	if loc := projectRootRe.FindStringIndex(file); loc != nil {
		start := loc[0]
		if file[start] == '/' {
			start++
		}
		return file[start:]
	}
	if i := strings.LastIndexByte(file, '/'); i >= 0 {
		return file[i+1:]
	}
	return file
}

// frameKey identifies a frame without its line number.
func frameKey(f Frame) string {
	switch {
	case f.Class != "" && f.Function != "":
		return f.Class + "::" + f.Function
	case f.Function != "" && f.File != "":
		return NormalizePath(f.File) + ":" + f.Function
	case f.Function != "":
		return f.Function
	default:
		return NormalizePath(f.File)
	}
}

// significantFrames returns up to n in-app frames, or the top n frames if
// the trace has no application code at all (for example, a framework error).
func significantFrames(frames []Frame, n int) []Frame {
	out := make([]Frame, 0, n)
	for _, f := range frames {
		if IsInApp(f) {
			out = append(out, f)
			if len(out) == n {
				return out
			}
		}
	}
	if len(out) > 0 {
		return out
	}
	if len(frames) > n {
		return frames[:n]
	}
	return frames
}

// Fingerprint groups exceptions: same class, same normalized message and
// same in-app call path → same issue.
func Fingerprint(exceptionType, message string, frames []Frame) string {
	h := sha256.New()
	write := func(s string) { h.Write([]byte(s)); h.Write([]byte{0}) }
	write("v1")
	write(exceptionType)
	write(NormalizeMessage(message))
	for _, f := range significantFrames(frames, fingerprintFrames) {
		write(frameKey(f))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// QueueFingerprint groups queue failures by job class plus exception
// fingerprint (PRD §4.5).
func QueueFingerprint(jobName, exceptionFingerprint string) string {
	sum := sha256.Sum256([]byte("queue\x00" + jobName + "\x00" + exceptionFingerprint))
	return hex.EncodeToString(sum[:])
}

// Culprit names the most relevant frame for list views, such as
// "App\Http\Controllers\OrderController::store" or "app/Services/Checkout.php:88".
func Culprit(frames []Frame) string {
	sig := significantFrames(frames, 1)
	if len(sig) == 0 {
		return ""
	}
	f := sig[0]
	if f.Class != "" && f.Function != "" {
		return f.Class + "::" + f.Function
	}
	if f.File != "" {
		loc := NormalizePath(f.File)
		if f.Line > 0 {
			loc += ":" + strconv.Itoa(f.Line)
		}
		return loc
	}
	return f.Function
}
