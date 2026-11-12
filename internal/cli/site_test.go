package cli

import (
	"encoding/json"
	"html"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/ChrAlpha/tmux-agent-experience/internal/adapter"
	"github.com/ChrAlpha/tmux-agent-experience/internal/query"
)

// sitePages are the landing page and its translations. The first is the
// source the others follow.
var sitePages = []string{"site/index.html", "site/zh-CN/index.html"}

func readRepo(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", path))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

var (
	siteTag     = regexp.MustCompile(`<[^>]*>`)
	siteComment = regexp.MustCompile(`<span class="c">[^<]*</span>`)
	sitePre     = regexp.MustCompile(`(?s)<pre[^>]*>(.*?)</pre>`)
	siteInline  = regexp.MustCompile(`(?s)<(?:code|samp)[^>]*>(.*?)</(?:code|samp)>`)
)

// siteText is what a fragment of the page reads as.
func siteText(fragment string) string {
	return html.UnescapeString(siteTag.ReplaceAllString(fragment, ""))
}

// siteCode is a page's code: its preformatted blocks without their
// comments, which a translation may change, its inline code, and all of it as
// one text.
func siteCode(page string) (blocks, inline []string, all string) {
	for _, m := range sitePre.FindAllStringSubmatch(page, -1) {
		blocks = append(blocks, siteText(siteComment.ReplaceAllString(m[1], "")))
	}
	all = strings.Join(blocks, "\n")
	for _, m := range siteInline.FindAllStringSubmatch(sitePre.ReplaceAllString(page, ""), -1) {
		text := siteText(m[1])
		inline = append(inline, text)
		all += "\n" + text
	}
	return blocks, inline, all
}

// clientUsage is tmag-client's usage, as its entry point prints it.
func clientUsage(t *testing.T) string {
	t.Helper()
	src := readRepo(t, "client/cmd/tmag-client/main.go")
	m := regexp.MustCompile("(?s)const usage = `(.*?)`").FindStringSubmatch(src)
	if m == nil {
		t.Fatal("found no tmag-client usage in client/cmd/tmag-client/main.go")
	}
	return m[1]
}

// The landing page teaches tmag as it is, like the peers skill: the
// commands, flags, options, keys and output it shows are the binary's and the
// plugin's, its install prompt is README's, and a translation changes its
// words, never its code.
func TestSiteTeachesTmagAsItIs(t *testing.T) {
	plugin := readRepo(t, "tmag.tmux") + readRepo(t, "scripts/install.sh")
	commands := usageCommands()
	client := clientUsage(t)
	clientCommands := map[string]bool{}
	for _, m := range regexp.MustCompile(`(?m)^  tmag-client ((?:auth )?[a-z][a-z-]*)`).FindAllStringSubmatch(client, -1) {
		clientCommands[m[1]] = true
	}

	// The plugin's default keys, TPM's install and update, and tmux's keys to
	// the next and previous window with an alert.
	keys := map[string]bool{"I": true, "U": true, "M-n": true, "M-p": true}
	for _, m := range regexp.MustCompile(`option @tmag-key-[a-z]+ (\S+)\)`).FindAllStringSubmatch(plugin, -1) {
		keys[m[1]] = true
	}

	var firstLines []*regexp.Regexp
	for _, m := range regexp.MustCompile(`Sprintf\("(%s (?:From|Reply from) %s[^"]*)"`).FindAllStringSubmatch(readRepo(t, "internal/query/send.go"), -1) {
		firstLines = append(firstLines, regexp.MustCompile("^"+strings.ReplaceAll(regexp.QuoteMeta(m[1]), "%s", ".+")+"$"))
	}
	if len(firstLines) == 0 {
		t.Fatal("found no first lines in query/send.go")
	}

	header := regexp.MustCompile(`"(TARGET\\t[^"]+)"`).FindStringSubmatch(readRepo(t, "internal/cli/commands.go"))
	if header == nil {
		t.Fatal("found no tmag list header in cli/commands.go")
	}
	columns := strings.Split(header[1], `\t`)

	request := regexp.MustCompile(`"(request: [^"]*)\\n"`).FindStringSubmatch(readRepo(t, "internal/cli/commands.go"))
	if request == nil {
		t.Fatal("found no tmag inspect request line in cli/commands.go")
	}
	requestLine := regexp.MustCompile("^" + strings.ReplaceAll(regexp.QuoteMeta(request[1]), "%s", ".+") + "$")

	summary := Summary([]query.Session{{State: adapter.Blocked}, {State: adapter.Working}, {State: adapter.Idle, Turn: "turn"}}, false)

	prompt := regexp.MustCompile(`(?m)^> (Read https://\S+/SKILL\.md .+)$`)
	readmePrompts := map[string]bool{}
	for _, readme := range []string{"README.md", "README.zh-CN.md"} {
		m := prompt.FindStringSubmatch(readRepo(t, readme))
		if m == nil {
			t.Fatalf("%s has no install prompt", readme)
		}
		readmePrompts[m[1]] = true
	}
	if len(readmePrompts) != 1 {
		t.Fatalf("README.md and README.zh-CN.md give different install prompts: %v", readmePrompts)
	}

	var source []string
	for i, path := range sitePages {
		page := readRepo(t, path)
		blocks, inline, code := siteCode(page)
		if i == 0 {
			source = blocks
		} else if !slices.Equal(blocks, source) {
			t.Errorf("%s: its code blocks differ from %s's; a translation changes only comments", path, sitePages[0])
		}

		m := regexp.MustCompile(`(?s)<p id="install-prompt">(.*?)</p>`).FindStringSubmatch(page)
		if m == nil || !readmePrompts[siteText(m[1])] {
			t.Errorf("%s: the install prompt is not README's", path)
		}

		for _, c := range regexp.MustCompile(`\btmag ([a-z][a-z-]*)`).FindAllStringSubmatch(code, -1) {
			if !commands[c[1]] {
				t.Errorf("%s names tmag %s, which the CLI does not have", path, c[1])
			}
		}
		for _, c := range regexp.MustCompile(`\btmag-client ((?:auth )?[a-z][a-z-]*)`).FindAllStringSubmatch(code, -1) {
			if !clientCommands[c[1]] {
				t.Errorf("%s names tmag-client %s, which the client does not have", path, c[1])
			}
		}
		for _, o := range regexp.MustCompile(`@tmag-[a-z]+(?:-[a-z]+)*`).FindAllString(siteText(page), -1) {
			if !strings.Contains(plugin, o) {
				t.Errorf("%s names %s, which the tmux plugin does not have", path, o)
			}
		}
		named := regexp.MustCompile(`<kbd>prefix</kbd> <kbd>([^<]+)</kbd>`).FindAllStringSubmatch(page, -1)
		named = append(named, regexp.MustCompile(`prefix \+ (\S+)`).FindAllStringSubmatch(code, -1)...)
		for _, k := range named {
			if !keys[k[1]] {
				t.Errorf("%s names prefix + %s, which neither the plugin, TPM nor tmux binds", path, k[1])
			}
		}

		clientLine := false
		previous := ""
		for _, raw := range strings.Split(strings.Join(blocks, "\n"), "\n") {
			line := strings.TrimSpace(raw)
			switch {
			case strings.HasPrefix(line, "tmag-client"):
				clientLine = true
			case !clientLine, !strings.HasSuffix(previous, `\`):
				clientLine = false
			}
			// A client command names the client's flags, continued lines included;
			// every other line names tmag's.
			taken := usage
			if clientLine {
				taken = client
			}
			for _, f := range regexp.MustCompile(`(?:^|[\s(])(--[a-z][a-z-]*)`).FindAllStringSubmatch(line, -1) {
				if !regexp.MustCompile(regexp.QuoteMeta(f[1]) + `\b`).MatchString(taken) {
					t.Errorf("%s names %s, which no command takes", path, f[1])
				}
			}
			previous = line
		}
		// Inline code names a command or a flag from either tool.
		for _, fragment := range inline {
			for _, f := range regexp.MustCompile(`(?:^|[\s(])(--[a-z][a-z-]*)`).FindAllStringSubmatch(fragment, -1) {
				if !regexp.MustCompile(regexp.QuoteMeta(f[1])+`\b`).MatchString(usage) && !regexp.MustCompile(regexp.QuoteMeta(f[1])+`\b`).MatchString(client) {
					t.Errorf("%s names %s, which no command takes", path, f[1])
				}
			}
		}
		for _, line := range strings.Split(code, "\n") {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, query.Marker) && !slices.ContainsFunc(firstLines, func(re *regexp.Regexp) bool { return re.MatchString(line) }) {
				t.Errorf("%s shows a first line query.Send never writes: %s", path, line)
			}
			if strings.HasPrefix(line, "TARGET ") && !slices.Equal(strings.Fields(line), columns) {
				t.Errorf("%s shows tmag list's columns as %q, want %q", path, strings.Fields(line), columns)
			}
			if strings.HasPrefix(line, "request: ") && !requestLine.MatchString(line) {
				t.Errorf("%s shows an open request as tmag inspect never prints one: %s", path, line)
			}
		}
		if !strings.Contains(siteText(page), summary) {
			t.Errorf("%s does not show tmag status as it reads: %q", path, summary)
		}
	}
}

// The demo settles a finished agent its jump visits: the next jump leaves it,
// and the status line counts it idle. site.js exposes that logic to Node
// without changing the page.
func TestSiteDemoSettlesVisitedTurns(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	site, err := filepath.Abs(filepath.Join("..", "..", "site", "site.js"))
	if err != nil {
		t.Fatal(err)
	}
	const script = `
const { jumpNext, jumpVisit, jumpCounts } = require(process.env.SITE);
const agents = [
  { window: 0, state: "blocked", since: -12 },
  { window: 1, state: "working", since: -60 },
  { window: 2, state: "finished", since: -180 },
];
let current = 1;
const visits = [];
for (let i = 0; i < 4; i++) {
  const target = jumpVisit(agents, current);
  visits.push(target ? target.window : null);
  if (target) current = target.window;
}
const next = jumpNext(agents, current);
console.log(JSON.stringify({ visits, counts: jumpCounts(agents), next: next ? next.window : null }));
`
	cmd := exec.Command(node, "-e", script)
	cmd.Env = append(os.Environ(), "SITE="+site)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("site demo: %v\n%s", err, out)
	}
	var got struct {
		Visits []*int         `json:"visits"`
		Counts map[string]int `json:"counts"`
		Next   *int           `json:"next"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("site demo output %q: %v", out, err)
	}
	want := []int{0, 2, 0, -1}
	if len(got.Visits) != len(want) {
		t.Fatalf("visits = %v, want %v", got.Visits, want)
	}
	for i, w := range want {
		n := -1
		if got.Visits[i] != nil {
			n = *got.Visits[i]
		}
		if n != w {
			t.Fatalf("visits = %v, want %v", got.Visits, want)
		}
	}
	if got.Next != nil {
		t.Fatalf("jump after the visits still takes %d", *got.Next)
	}
	for state, n := range map[string]int{"blocked": 1, "finished": 0, "working": 1, "idle": 1} {
		if got.Counts[state] != n {
			t.Fatalf("counts = %v, want blocked 1, finished 0, working 1, idle 1", got.Counts)
		}
	}
}
