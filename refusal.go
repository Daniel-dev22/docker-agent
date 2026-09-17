package main

// Refusals — every deterministic 4xx a mutating route returns.
//
// Two rules live here, in one place, because every refusal must obey both:
//
//   - A refusal carries a machine-readable `code`, and a coded 4xx is FINAL:
//     consumers dead-letter it (the controller's shared refusal rule), and treat a
//     4xx WITHOUT a code as transient (retried ~100x). A refusal whose answer can
//     change on retry says so with "retryable": true, and only the codes in
//     retryableCodes may; a failure that is not the caller's fault (I/O) is a 5xx
//     with no code.
//   - A refusal never echoes caller input unbounded. Refusals are recorded verbatim
//     by the idempotency store, and a request naming a 1 MB project got a 2 MB 400
//     back — recorded, twice, per request. Every echoed value is clipped to
//     echoMax and the message to refusalMessageMax.

import (
	"fmt"
	"net/http"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
)

const (
	// echoMax bounds one echoed caller value. Real values are far shorter: the
	// longest registered path on the fleet is 81 bytes and the longest project
	// name 19 (all 10 agents, 2026-09-17); 512 keeps any realistic path whole.
	echoMax = 512
	// refusalMessageMax bounds a whole refusal message (a few echoes plus prose).
	refusalMessageMax = 2048
	// maxProjectNameLen is the filesystem's NAME_MAX: a project's files live in a
	// directory of that name, so a longer name can only fail — as an I/O error
	// that would read as transient.
	maxProjectNameLen = 255
	// maxPathLen is Linux PATH_MAX.
	maxPathLen = 4096
)

// retryableCodes are the only codes a 4xx may mark "retryable": true.
var retryableCodes = map[string]bool{
	// The same key is still being handled; a retry gets its recorded answer.
	"idempotency_key_in_flight": true,
}

// echo clips a caller-supplied value for inclusion in a refusal.
func echo(s string) string { return clipTo(s, echoMax) }

func clipTo(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + fmt.Sprintf("…(+%d bytes)", len(s)-cut)
}

// refuse writes a refusal: status, a required code, the clipped message, and extra
// fields whose string values are clipped as echoes.
func refuse(c *gin.Context, status int, code, msg string, fields gin.H) {
	if code == "" {
		// A programming error, and the one kind of refusal that must not ship.
		panic("refuse: empty code for " + http.StatusText(status))
	}
	if fields["retryable"] == true && !retryableCodes[code] {
		panic("refuse: " + code + " is not a retryable code")
	}
	body := gin.H{"error": clipTo(msg, refusalMessageMax), "code": code}
	for k, v := range fields {
		switch t := v.(type) {
		case string:
			body[k] = echo(t)
		case []string:
			clipped := make([]string, len(t))
			for i, s := range t {
				clipped[i] = echo(s)
			}
			body[k] = clipped
		default:
			body[k] = v
		}
	}
	c.AbortWithStatusJSON(status, body)
}
