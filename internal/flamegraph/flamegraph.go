// Package flamegraph renders folded stacks as an off-CPU flame graph.
//
// The width of a frame here is not how long that code ran. It is how long a
// thread sat still inside it, which is the half of wall clock that a CPU
// profiler cannot draw at all.
//
// # Why this is not flamegraph.pl
//
// The usual renderer colours a frame by hashing its name, because a folded
// file is all it is given and a name is all it knows. This tool classified
// every one of these stacks before it wrote them out -- network, futex, disk,
// sleep, poll -- and discarding that at the last step would throw away the
// one thing it knows that a generic renderer cannot. Here the colour is the
// reason, and the legend states each category's share.
//
// That matters more than it sounds, because of where the picture ends up. An
// SVG embedded in a README is served as a flat image: no hover, no click, no
// script. Random colours say nothing under those conditions and the reader
// sees a pretty shape. A legend survives being a screenshot, so the image
// still answers "what was this process waiting for" with no interaction at
// all.
package flamegraph

import (
	"bufio"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/FranciscoPLoureiro/wallclock/internal/offcpu"
)

// Options controls the rendering. The zero value is usable: every field
// falls back to a default.
type Options struct {
	// Title is the heading drawn at the top left.
	Title string
	// Subtitle is drawn under it. The measured totals are appended to
	// whatever is given here, so it is for naming the subject: which
	// process, which window.
	Subtitle string
	// Width is the image width in pixels.
	Width int
}

// Layout. Everything is derived from these, so the picture scales by
// changing the width alone.
const (
	defaultWidth = 1200
	frameHeight  = 17
	fontSize     = 12
	titleSize    = 17
	subtitleSize = 12
	marginSide   = 10
	marginTop    = 56 // title and subtitle
	marginBottom = 54 // legend

	// charWidth is how wide one character of the monospace face is at
	// fontSize. Near enough to decide whether a label fits; a wrong guess
	// costs a slightly short or slightly clipped label and nothing more.
	charWidth = 0.6 * fontSize

	// minFraction is the narrowest frame worth drawing, as a share of the
	// total. Below this a rectangle is under a pixel wide and only adds
	// bytes, and there are a great many of them: the tail of a stack tree
	// is almost all of its nodes.
	minFraction = 0.001
)

// reasonOrder fixes the order categories appear in the legend, and breaks
// ties when a frame holds two reasons in equal measure. Without it the same
// input could render two different pictures, and this SVG is committed to
// the repository: a regenerated file has to differ only when the measurement
// differs.
var reasonOrder = []offcpu.Reason{
	offcpu.ReasonNetwork,
	offcpu.ReasonFutex,
	offcpu.ReasonPoll,
	offcpu.ReasonDisk,
	offcpu.ReasonSleep,
	offcpu.ReasonOther,
}

// reasonHue is the colour each category is drawn in: hue and saturation
// fixed per reason, lightness jittered per frame so that neighbouring
// rectangles of one category are still told apart.
var reasonHue = map[offcpu.Reason][2]int{
	offcpu.ReasonNetwork: {209, 68},
	offcpu.ReasonFutex:   {26, 82},
	offcpu.ReasonPoll:    {142, 42},
	offcpu.ReasonDisk:    {283, 44},
	offcpu.ReasonSleep:   {212, 12},
	offcpu.ReasonOther:   {45, 34},
}

// node is one frame of the merged stack tree.
type node struct {
	name  string
	value int64 // microseconds, this frame and everything above it
	// byReason splits value by what the stacks passing through this frame
	// were waiting for. An interior frame is shared -- __schedule sits under
	// every wait there is -- so one reason per node would be a lie for
	// exactly the frames nearest the root.
	byReason map[offcpu.Reason]int64
	children map[string]*node
}

func newNode(name string) *node {
	return &node{
		name:     name,
		byReason: make(map[offcpu.Reason]int64),
		children: make(map[string]*node),
	}
}

// add walks one folded stack into the tree.
func (n *node) add(frames []string, reason offcpu.Reason, micros int64) {
	cur := n
	cur.value += micros
	cur.byReason[reason] += micros
	for _, frame := range frames {
		child, ok := cur.children[frame]
		if !ok {
			child = newNode(frame)
			cur.children[frame] = child
		}
		child.value += micros
		child.byReason[reason] += micros
		cur = child
	}
}

// dominant is the category holding the most time in this frame. Ties are
// broken by reasonOrder rather than by map order, which is random.
func (n *node) dominant() offcpu.Reason {
	best, bestTotal := offcpu.ReasonOther, int64(0)
	for _, reason := range reasonOrder {
		if total := n.byReason[reason]; total > bestTotal {
			best, bestTotal = reason, total
		}
	}
	return best
}

// sortedChildren returns the children by name, so that one measurement
// always draws one picture.
func (n *node) sortedChildren() []*node {
	out := make([]*node, 0, len(n.children))
	for _, child := range n.children {
		out = append(out, child)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out
}

// depth is how many rows this subtree needs, itself included.
func (n *node) depth() int {
	deepest := 0
	for _, child := range n.children {
		if d := child.depth(); d > deepest {
			deepest = d
		}
	}
	return deepest + 1
}

// Render reads folded stacks -- comm;outermost;...;innermost microseconds,
// the format writeFolded produces and flamegraph.pl reads -- and writes an
// SVG flame graph.
func Render(out io.Writer, in io.Reader, opts Options) error {
	root, stacks, err := parse(in)
	if err != nil {
		return err
	}
	if root.value == 0 {
		return errors.New("no off-CPU stacks to draw: nothing blocked during the window")
	}

	width := opts.Width
	if width <= 0 {
		width = defaultWidth
	}
	title := opts.Title
	if title == "" {
		title = "off-CPU flame graph"
	}
	rows := root.depth()
	height := marginTop + rows*frameHeight + marginBottom
	plot := float64(width - 2*marginSide)

	var b strings.Builder
	fmt.Fprintf(&b, `<svg xmlns="http://www.w3.org/2000/svg" width="%d" height="%d" `+
		`viewBox="0 0 %d %d" `+
		`font-family="ui-monospace, SFMono-Regular, Menlo, Consolas, monospace">`+"\n",
		width, height, width, height)
	fmt.Fprintf(&b, `<rect width="%d" height="%d" fill="#f7f7f5"/>`+"\n", width, height)
	fmt.Fprintf(&b, `<text x="%d" y="24" font-size="%d" fill="#16161a">%s</text>`+"\n",
		marginSide, titleSize, escape(title))
	fmt.Fprintf(&b, `<text x="%d" y="42" font-size="%d" fill="#5c5c66">%s</text>`+"\n",
		marginSide, subtitleSize, escape(summary(opts.Subtitle, root.value, stacks)))

	// The root is drawn with everything else: it is the row labelled "all"
	// along the bottom, and its width is the whole of the blocked time.
	draw(&b, root, float64(marginSide), plot, 0, rows, root.value)
	legend(&b, root, width, height)

	b.WriteString("</svg>\n")
	_, err = io.WriteString(out, b.String())
	return err
}

// draw emits one frame and recurses into its children, laying them out left
// to right in proportion to their time.
func draw(b *strings.Builder, n *node, x, w float64, depth, rows int, total int64) {
	y := float64(marginTop + (rows-1-depth)*frameHeight)

	fmt.Fprintf(b, `<g><title>%s</title>`, escape(tooltip(n, total)))
	fmt.Fprintf(b, `<rect x="%.1f" y="%.1f" width="%.1f" height="%d" rx="2" fill="%s"/>`,
		x, y, max(w-1, 0.4), frameHeight-1, colourOf(n.dominant(), n.name))
	if label := fit(n.name, w); label != "" {
		fmt.Fprintf(b, `<text x="%.1f" y="%.1f" font-size="%d" fill="#16161a">%s</text>`,
			x+3, y+float64(frameHeight)-5.5, fontSize, escape(label))
	}
	b.WriteString("</g>\n")

	// Children are laid out against this frame's own width, so a subtree
	// keeps its shape wherever it sits. The cut is on the share of the whole
	// rather than on the share of the parent: what cannot be seen is what is
	// not worth emitting, and a narrow frame stays narrow however deep it is.
	cutoff := minFraction * float64(total)
	offset := x
	for _, child := range n.sortedChildren() {
		cw := w * float64(child.value) / float64(n.value)
		if float64(child.value) >= cutoff {
			draw(b, child, offset, cw, depth+1, rows, total)
		}
		offset += cw
	}
}

// legend draws the swatches, and with them the finding: which categories the
// waiting was in, and in what proportion. This is what makes the picture
// readable with no interaction, so it is not decoration.
func legend(b *strings.Builder, root *node, width, height int) {
	y := float64(height - marginBottom + 20)
	fmt.Fprintf(b, `<text x="%d" y="%.1f" font-size="%d" fill="#5c5c66">`+
		`waiting on:</text>`+"\n", marginSide, y, fontSize)

	x := float64(marginSide) + 11*charWidth
	for _, reason := range reasonOrder {
		total := root.byReason[reason]
		if total == 0 {
			continue
		}
		label := fmt.Sprintf("%s %.1f%%", reason, 100*float64(total)/float64(root.value))
		fmt.Fprintf(b, `<rect x="%.1f" y="%.1f" width="11" height="11" rx="2" fill="%s"/>`,
			x, y-10, colourOf(reason, string(reason)))
		fmt.Fprintf(b, `<text x="%.1f" y="%.1f" font-size="%d" fill="#16161a">%s</text>`+"\n",
			x+16, y, fontSize, escape(label))
		x += 16 + float64(len(label)+3)*charWidth
	}

	// Said on the image itself, because the image travels without the README
	// around it, and an off-CPU flame graph is read wrongly by anybody who
	// assumes the usual kind.
	fmt.Fprintf(b, `<text x="%d" y="%.1f" font-size="%d" fill="#5c5c66">`+
		`width is time spent stopped inside a frame, not time spent running it`+
		`</text>`+"\n", marginSide, float64(height-marginBottom+40), fontSize)
}

// tooltip is what hovering a frame says. It is only reachable when the SVG is
// opened on its own -- GitHub serves it as a flat image -- which is why the
// same information is in the legend.
func tooltip(n *node, total int64) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "%s\n%s (%.2f%%)", n.name,
		time.Duration(n.value)*time.Microsecond,
		100*float64(n.value)/float64(total))
	for _, reason := range reasonOrder {
		if v := n.byReason[reason]; v > 0 {
			fmt.Fprintf(&sb, "\n  %-8s %s", reason, time.Duration(v)*time.Microsecond)
		}
	}
	return sb.String()
}

// summary is the subtitle: whatever the caller named the subject, plus what
// was actually measured.
func summary(subtitle string, micros int64, stacks int) string {
	measured := fmt.Sprintf("%s blocked across %d stacks",
		time.Duration(micros)*time.Microsecond, stacks)
	if subtitle == "" {
		return measured
	}
	return subtitle + " -- " + measured
}

// colourOf is the category's hue, with the lightness jittered by the frame
// name so that two neighbouring rectangles of one category are still two
// rectangles. The jitter is a hash rather than a random number, because the
// rendered file is committed and has to be reproducible.
func colourOf(reason offcpu.Reason, name string) string {
	hs, ok := reasonHue[reason]
	if !ok {
		hs = reasonHue[offcpu.ReasonOther]
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(name))
	lightness := 62 + int(h.Sum32()%13) - 6 //nolint:gosec // 0..12, then centred
	return fmt.Sprintf("hsl(%d,%d%%,%d%%)", hs[0], hs[1], lightness)
}

// fit shortens a label to what the rectangle can hold, or returns empty if
// it can hold nothing worth reading.
//
// Counted in runes rather than bytes. Kernel symbols are ASCII, but the
// bottom row of every tower is a thread name, and that is fifteen bytes of
// whatever the process put there -- cutting one in half mid-rune would emit
// a broken sequence into the document.
func fit(name string, w float64) string {
	room := int((w - 6) / charWidth)
	if room < 3 {
		return ""
	}
	runes := []rune(name)
	if len(runes) <= room {
		return name
	}
	return string(runes[:room-2]) + ".."
}

// parse reads the folded file into a tree, and returns how many stacks it
// held.
func parse(r io.Reader) (*node, int, error) {
	root := newNode("all")
	scanner := bufio.NewScanner(r)
	// Kernel stacks are up to 32 frames of symbol names, so the default
	// 64 KB token is ample -- but a folded file is not necessarily one this
	// tool wrote, and the failure would be a truncated line parsed as a
	// short stack rather than an error.
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	stacks := 0
	for line := 1; scanner.Scan(); line++ {
		text := strings.TrimSpace(scanner.Text())
		if text == "" {
			continue
		}
		cut := strings.LastIndexByte(text, ' ')
		if cut < 0 {
			return nil, 0, fmt.Errorf("line %d: no count on the end of %q", line, text)
		}
		micros, err := strconv.ParseInt(text[cut+1:], 10, 64)
		if err != nil {
			return nil, 0, fmt.Errorf("line %d: %q is not a count: %w", line, text[cut+1:], err)
		}
		if micros <= 0 {
			// A zero-width frame draws nothing, and a negative one would
			// make nonsense of every proportion above it.
			continue
		}
		frames := strings.Split(text[:cut], ";")

		// The first field is the thread name, and it is deliberately kept
		// from the classifier: a process called tcp-relay would be filed as
		// network waiting on the strength of its own name.
		root.add(frames, offcpu.Classify(innermostFirst(frames[1:])), micros)
		stacks++
	}
	if err := scanner.Err(); err != nil {
		return nil, 0, fmt.Errorf("reading folded stacks: %w", err)
	}
	return root, stacks, nil
}

// innermostFirst reverses a folded stack into the order Classify expects.
// The folded format is outermost first; the classifier scans from the
// deepest frame outwards, because the deepest call that means anything is
// the one that decided to stop.
func innermostFirst(frames []string) []string {
	out := make([]string, len(frames))
	for i, frame := range frames {
		out[len(frames)-1-i] = frame
	}
	return out
}

// escape makes a string safe to put inside an XML text node.
//
// It does more than the five entities, and the reason is where these strings
// come from. Frame names are kernel symbols and are tame, but the first field
// of every folded line is a thread name -- fifteen bytes the process chose,
// which need not be valid UTF-8 and need not be printable. XML 1.0 forbids
// most control characters outright, so a single stray byte in a comm would
// produce a document no renderer will open.
//
// Ranging over a string already turns each byte of an invalid sequence into
// U+FFFD, which is legal and visible in the picture; what is left to do here
// is the control characters, which are neither.
func escape(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r == '&':
			b.WriteString("&amp;")
		case r == '<':
			b.WriteString("&lt;")
		case r == '>':
			b.WriteString("&gt;")
		case r == '"':
			b.WriteString("&quot;")
		case r == '\n' || r == '\t':
			// Legal in XML, and the tooltips are deliberately multi-line.
			b.WriteRune(r)
		case r < 0x20 || r == 0x7f:
			b.WriteRune('�')
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}
