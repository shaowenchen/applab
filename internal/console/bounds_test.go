package console

import (
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/shaowenchen/applab/internal/model"
)

// TestBoundsMatchTheModel asserts the bounds the console enforces are the ones
// the API enforces.
//
// The console is a static file with no build step, so it cannot import the Go
// constants — which means the same two numbers are written down twice, once in
// model.ValidatePort and once in the page's JavaScript. Two copies of a bound is
// one that drifts, and this is the drift that hurts: a console offering a port
// the API then refuses reports a failure the reader cannot act on, because the
// field they typed into said the value was fine.
//
// It reads the numbers out of the page rather than restating them, so a change
// to either copy fails here rather than at the point someone loses an afternoon
// to the discrepancy.
func TestBoundsMatchTheModel(t *testing.T) {
	page, err := os.ReadFile("static/index.html")
	if err != nil {
		t.Fatalf("read the console page: %v", err)
	}
	html := string(page)

	// Every place the page states a port bound, in the form it states it:
	//
	//   the markup's min/max on the field
	//   the JavaScript's comparison against a typed value
	//   the message the reader is shown
	//
	// All three have to agree with model, and they are checked together because
	// a page whose field allowed a value its script refused would be as broken as
	// one whose script allowed a value the API refused.
	checks := []struct {
		name string
		re   *regexp.Regexp
		got  func(m []string) (int32, error)
		want func() int32
	}{
		{
			name: "the port field's min",
			re:   regexp.MustCompile(`id="app-port"[^>]*\bmin="(\d+)"`),
			got:  parseFirst,
			want: func() int32 { return model.MinPort },
		},
		{
			name: "the port field's max",
			re:   regexp.MustCompile(`id="app-port"[^>]*\bmax="(\d+)"`),
			got:  parseFirst,
			want: func() int32 { return model.MaxPort },
		},
		// The comparisons in both forms read the constants rather than repeating
		// the numbers, so these check the constants themselves — the one place
		// the page states a bound now.
		{
			name: "the port minimum",
			re:   regexp.MustCompile(`const MIN_PORT = (\d+)`),
			got:  parseFirst,
			want: func() int32 { return model.MinPort },
		},
		{
			name: "the port maximum",
			re:   regexp.MustCompile(`const MAX_PORT = (\d+)`),
			got:  parseFirst,
			want: func() int32 { return model.MaxPort },
		},
		{
			name: "the replica maximum",
			re:   regexp.MustCompile(`const MAX_REPLICAS = (\d+)`),
			got:  parseFirst,
			want: func() int32 { return model.MaxReplicas },
		},
	}

	for _, tc := range checks {
		t.Run(tc.name, func(t *testing.T) {
			m := tc.re.FindStringSubmatch(html)
			if m == nil {
				t.Fatalf("the page no longer states %s in the form this check reads (%s); "+
					"if the expression changed deliberately, update this check", tc.name, tc.re)
			}
			got, err := tc.got(m)
			if err != nil {
				t.Fatalf("read %s: %v", tc.name, err)
			}
			if want := tc.want(); got != want {
				t.Errorf("the console allows %s = %d, but the API allows %d — "+
					"a value the page accepts and the server refuses is a failure the reader cannot act on",
					tc.name, got, want)
			}
		})
	}

	// The message has to carry the same numbers, because it is what someone acts
	// on. A page that refused 70000 with "between 1 and 65535" while the API
	// said something else would be two different explanations of one refusal.
	// Both bounds are stated as templates with the numbers as placeholders, so
	// the console fills in the same values the API would have written. Checked as
	// the shape rather than as a sentence, because the interpolation is the part
	// that makes the two agree.
	for _, want := range []string{
		"Port {port} must be a number between {min} and {max}.",
		"Replicas must be at least 1, not {n}; use the stop endpoint to scale an app down.",
		"Replicas {n} exceeds the maximum of {max}.",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("the console does not state %q; the API says exactly that, and the two are read by the same person", want)
		}
	}
}

func parseFirst(m []string) (int32, error)  { return parseInt(m[1]) }
func parseSecond(m []string) (int32, error) { return parseInt(m[2]) }

func parseInt(s string) (int32, error) {
	n, err := strconv.ParseInt(s, 10, 32)
	return int32(n), err
}
