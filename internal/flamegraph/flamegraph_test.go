package flamegraph

import (
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/FranciscoPLoureiro/wallclock/internal/offcpu"
)

// render is the whole package in one call, for tests that only care about
// what came out.
func render(t *testing.T, folded string, opts Options) string {
	t.Helper()
	var out strings.Builder
	if err := Render(&out, strings.NewReader(folded), opts); err != nil {
		t.Fatalf("Render: %v", err)
	}
	return out.String()
}

// rectWidth pulls every frame width out of the document, in order.
//
// Anchored on the frame height, and that is not incidental: the legend draws
// rectangles too, and a pattern that only looked for a width counted its
// swatches as frames. The first version of this helper did, and reported
// four rectangles where the picture has three.
var rectWidth = regexp.MustCompile(
	fmt.Sprintf(`<rect x="[0-9.]+" y="[0-9.]+" width="([0-9.]+)" height="%d"`, frameHeight-1))

func widths(svg string) []float64 {
	var out []float64
	for _, m := range rectWidth.FindAllStringSubmatch(svg, -1) {
		w, _ := strconv.ParseFloat(m[1], 64)
		out = append(out, w)
	}
	return out
}

func TestStacksSharingAPrefixMerge(t *testing.T) {
	root, stacks, err := parse(strings.NewReader(
		"app;entry;do_syscall;futex_wait 300\n" +
			"app;entry;do_syscall;tcp_recvmsg 100\n"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if stacks != 2 {
		t.Errorf("stacks = %d, want 2", stacks)
	}
	if root.value != 400 {
		t.Errorf("root value = %d, want 400", root.value)
	}

	shared := root.children["app"].children["entry"].children["do_syscall"]
	if shared == nil {
		t.Fatal("the shared prefix did not merge into one frame")
	}
	if shared.value != 400 {
		t.Errorf("shared frame value = %d, want 400", shared.value)
	}
	if got := len(shared.children); got != 2 {
		t.Errorf("shared frame has %d children, want 2", got)
	}

	// The shared frame carries both reasons, because it is genuinely under
	// both. Collapsing it to one would be the lie this split exists to
	// avoid.
	if got := shared.byReason[offcpu.ReasonFutex]; got != 300 {
		t.Errorf("futex under the shared frame = %d, want 300", got)
	}
	if got := shared.byReason[offcpu.ReasonNetwork]; got != 100 {
		t.Errorf("network under the shared frame = %d, want 100", got)
	}
	if got := shared.dominant(); got != offcpu.ReasonFutex {
		t.Errorf("dominant reason = %q, want futex", got)
	}
}

// The thread name is the first field of a folded line and it is not part of
// the stack. Handing it to the classifier would let a process be filed under
// a category because of what somebody called it.
func TestTheThreadNameIsNotClassified(t *testing.T) {
	root, _, err := parse(strings.NewReader("tcp_relay;entry;futex_wait 100\n"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got := root.dominant(); got != offcpu.ReasonFutex {
		t.Errorf("a thread called tcp_relay waiting in futex_wait was classified %q, want futex", got)
	}
}

// The SVG is committed to the repository, so regenerating it must produce
// the same bytes unless the measurement changed. Map iteration order is
// random in Go and would otherwise leak into the file.
func TestTheSameInputAlwaysDrawsTheSamePicture(t *testing.T) {
	const folded = "app;entry;futex_wait 300\n" +
		"app;entry;tcp_recvmsg 200\n" +
		"app;entry;ep_poll 150\n" +
		"other;entry;io_schedule 90\n"

	first := render(t, folded, Options{Title: "t"})
	for i := range 8 {
		if next := render(t, folded, Options{Title: "t"}); next != first {
			t.Fatalf("render %d differs from the first", i+2)
		}
	}
}

func TestWidthIsProportionalToTime(t *testing.T) {
	// Three parts to one, under a common parent.
	svg := render(t, "app;a 300\napp;b 100\n", Options{Width: 1000})

	got := widths(svg)
	// all, app, a, b -- the tree is walked depth first and children come
	// out sorted by name.
	if len(got) != 4 {
		t.Fatalf("drew %d rectangles, want 4: %v", len(got), got)
	}
	full := got[0]
	if diff := got[1] - full; diff > 0.6 || diff < -0.6 {
		t.Errorf("the single process is %.1f wide against a root of %.1f", got[1], full)
	}
	// 3:1, allowing for the pixel each rectangle gives up as a gap.
	if ratio := got[2] / got[3]; ratio < 2.9 || ratio > 3.1 {
		t.Errorf("300us against 100us drew %.1f and %.1f, a ratio of %.2f, want 3",
			got[2], got[3], ratio)
	}
}

func TestFramesTooNarrowToSeeAreNotDrawn(t *testing.T) {
	// One frame at a thousandth of the total is exactly at the cut; one at
	// half that is under it.
	svg := render(t, "app;wide 100000\napp;hairline 40\n", Options{Width: 1000})

	if strings.Contains(svg, "hairline") {
		t.Error("a frame of 0.04% was drawn; it is a fifth of a pixel wide")
	}
	if !strings.Contains(svg, "wide") {
		t.Error("the frame holding all the time was dropped")
	}
	// Dropping it must not move what comes after it: the parent's width is
	// still the whole of its subtree.
	if got := widths(svg); len(got) != 3 {
		t.Errorf("drew %d rectangles, want 3 (all, app, wide): %v", len(got), got)
	}
}

// A thread name is fifteen bytes of whatever the process put there. It
// reaches the document unfiltered unless something stops it, and XML has
// opinions about bytes.
func TestAThreadNameCannotBreakTheDocument(t *testing.T) {
	for _, name := range []string{
		"a<b&c",            // the entities
		"bell\x07here",     // a control character, illegal in XML 1.0
		"bad\xff\xfeutf8",  // not valid UTF-8
		"quote\"and'apost", // quoting
		strings.Repeat("x", 15),
	} {
		t.Run(strings.ToValidUTF8(name, "?"), func(t *testing.T) {
			svg := render(t, name+";entry;futex_wait 100\n", Options{Width: 400})
			if err := xml.Unmarshal([]byte(svg), new(struct {
				XMLName xml.Name
			})); err != nil {
				t.Fatalf("a thread called %q produced a document that will not parse: %v", name, err)
			}
			if strings.ContainsRune(svg, 0x07) {
				t.Error("a control character reached the document")
			}
		})
	}
}

func TestTheDocumentIsWellFormedXML(t *testing.T) {
	svg := render(t, "app;entry;tcp_recvmsg 500\napp;entry;futex_wait 250\n",
		Options{Title: "off-CPU", Subtitle: "a & b <c>"})

	decoder := xml.NewDecoder(strings.NewReader(svg))
	for {
		_, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("the document does not parse: %v", err)
		}
	}
}

// The legend is the whole reason this renderer exists rather than
// flamegraph.pl: it is what the picture says when it is served as a flat
// image with no hover.
func TestTheLegendStatesEachCategoryShare(t *testing.T) {
	svg := render(t, "app;entry;tcp_recvmsg 750\napp;entry;futex_wait 250\n", Options{})

	if !strings.Contains(svg, "network 75.0%") {
		t.Error("the legend does not carry the network share")
	}
	if !strings.Contains(svg, "futex 25.0%") {
		t.Error("the legend does not carry the futex share")
	}
	if strings.Contains(svg, "disk") {
		t.Error("a category with no time in it was given a swatch")
	}
}

func TestEachCategoryIsDrawnInItsOwnColour(t *testing.T) {
	seen := make(map[string]string)
	for _, reason := range reasonOrder {
		hue := strings.SplitN(strings.TrimPrefix(colourOf(reason, "x"), "hsl("), ",", 2)[0]
		if other, clash := seen[hue]; clash {
			t.Errorf("%q and %q are drawn at the same hue %s", reason, other, hue)
		}
		seen[hue] = string(reason)
	}
}

func TestMalformedInputIsRefusedRatherThanGuessed(t *testing.T) {
	for name, folded := range map[string]string{
		"no count":      "app;entry;futex_wait\n",
		"count is text": "app;entry;futex_wait later\n",
		"count is huge": "app;entry;futex_wait 99999999999999999999999\n",
	} {
		t.Run(name, func(t *testing.T) {
			err := Render(io.Discard, strings.NewReader(folded), Options{})
			if err == nil {
				t.Fatalf("%q was accepted", folded)
			}
			if !strings.Contains(err.Error(), "line 1") {
				t.Errorf("the error does not say which line: %v", err)
			}
		})
	}
}

func TestAnEmptyProfileSaysSoRatherThanDrawingNothing(t *testing.T) {
	err := Render(io.Discard, strings.NewReader("\n\n"), Options{})
	if err == nil {
		t.Fatal("an empty folded file rendered without complaint")
	}
	if !strings.Contains(err.Error(), "nothing blocked") {
		t.Errorf("the error does not explain what an empty file means: %v", err)
	}
}

// A zero count is a frame with no width, and a negative one would make
// nonsense of every proportion above it. Neither is an error in the file.
func TestCountsThatCannotBeDrawnAreSkipped(t *testing.T) {
	root, stacks, err := parse(strings.NewReader(
		"app;a 0\napp;b -5\napp;c 100\n"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if stacks != 1 {
		t.Errorf("counted %d stacks, want 1", stacks)
	}
	if root.value != 100 {
		t.Errorf("root value = %d, want 100", root.value)
	}
}

func TestTheImageIsExactlyAsTallAsItsDeepestStack(t *testing.T) {
	// all -> app -> entry -> futex_wait is four rows.
	svg := render(t, "app;entry;futex_wait 100\n", Options{})
	want := marginTop + 4*frameHeight + marginBottom
	if !strings.Contains(svg, fmt.Sprintf(`height="%d"`, want)) {
		t.Errorf("a four-row graph is not %d tall", want)
	}
}

func TestLabelsAreCutToTheirRectangle(t *testing.T) {
	tests := []struct {
		name  string
		width float64
		want  string
	}{
		{"futex_wait_queue_me", 1000, "futex_wait_queue_me"},
		{"futex_wait_queue_me", 60, "futex.."},
		{"futex_wait_queue_me", 12, ""},
		{"sched", 0.4, ""},
	}
	for _, tc := range tests {
		if got := fit(tc.name, tc.width); got != tc.want {
			t.Errorf("fit(%q, %.1f) = %q, want %q", tc.name, tc.width, got, tc.want)
		}
	}
}
