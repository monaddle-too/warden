package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// A side question (the composer's "/btw"): asked of a copy of the chat's
// Claude session, answered from its context, never entering it. The
// runner launches a second, one-shot CLI in the sandbox beside the
// resident one — `claude -p --resume <session> --fork-session` with no
// tools, the question on stdin (Claude Code copies the session under a
// new id it then discards from Warden's point of view) — with the
// streaming run's brokered environment, since the gateway serves the
// provider credential only for a live run, and reads the answer, its
// cost and its usage from the CLI's stream-json result
// (docs/claude-parity.md, item 15).

// MaxAsideQuestion bounds one question; AsideTimeout is how long the
// one-shot may run; asideOutputLimit is how much of the CLI's output the
// guest script keeps (its frames are a few kilobytes without tools).
const (
	MaxAsideQuestion = 16 << 10
	AsideTimeout     = 3 * time.Minute
	asideOutputLimit = 4 << 20
)

// A one-shot (the runner op "oneshot"; docs/claude-parity.md, R2.1): a
// fresh, tool-less CLI on a named model answering one prompt, with
// nothing resumed, nothing persisted and one model call at most — what a
// chat's automatic title is made with. OneShotTimeout bounds it; the
// prompt is bounded like a side question.
const OneShotTimeout = time.Minute

// AsideResult is what a side question came to: the answer, what the CLI
// said it cost, the session id of the copy it answered from, or the
// error when it could not answer.
type AsideResult struct {
	Text       string  `json:"text"`
	CostUSD    float64 `json:"costUSD,omitempty"`
	Input      int64   `json:"input,omitempty"`
	Output     int64   `json:"output,omitempty"`
	DurationMS int64   `json:"durationMS,omitempty"`
	SessionID  string  `json:"sessionID,omitempty"`
	Error      string  `json:"error,omitempty"`
	// Removed counts the session files the guest script removed after
	// the run (the fork's copy), for the log.
	Removed int `json:"removed,omitempty"`
}

// asideScript runs the one-shot CLI (its path and arguments after the
// script's own, plus `--tools ""`, which disables every tool and whose
// empty value the SBX exec API would refuse as an argument) with the
// question on stdin, the output kept to the cap, the process group
// killed at the timeout, and reports stdout, the last of stderr, the
// exit code and whether the timeout struck as one JSON line. A session
// the CLI wrote for the run (a fork's copy: the `system/init` or
// `result` frame names it, and it is not the one `--resume` named) is
// removed from the CLI's project directory once the answer is captured,
// so side questions leave no copies behind (R2.19); the report lists
// what was removed.
const asideScript = `import sys,os,json,subprocess,signal,glob,shutil
question,timeout,cap=sys.argv[1],float(sys.argv[2]),int(sys.argv[3])
argv=sys.argv[4:]
p=subprocess.Popen(argv+['--tools',''],stdin=subprocess.PIPE,stdout=subprocess.PIPE,stderr=subprocess.PIPE,start_new_session=True)
timed=False
try:
 out,err=p.communicate(question.encode(),timeout=timeout)
except subprocess.TimeoutExpired:
 timed=True
 try: os.killpg(p.pid,signal.SIGKILL)
 except OSError: pass
 out,err=p.communicate()
code=p.returncode
if code is None or code<0: code=-1
text=out[-cap:].decode('utf-8','replace')
keep=argv[argv.index('--resume')+1] if '--resume' in argv[:-1] else ''
sid=''
for line in text.splitlines():
 try: v=json.loads(line)
 except ValueError: continue
 if isinstance(v,dict) and v.get('type') in ('system','result') and isinstance(v.get('session_id'),str): sid=v['session_id']
removed=[]
if sid and sid!=keep and all(c in '0123456789abcdef-' for c in sid):
 root=os.environ.get('CLAUDE_CONFIG_DIR') or os.path.join(os.path.expanduser('~'),'.claude')
 for f in glob.glob(os.path.join(root,'projects','*',sid+'.jsonl')):
  try: os.remove(f); removed.append(f)
  except OSError: pass
 for d in glob.glob(os.path.join(root,'projects','*',sid)):
  if os.path.isdir(d): shutil.rmtree(d,ignore_errors=True); removed.append(d)
print(json.dumps({'output':text,'stderr':err[-4000:].decode('utf-8','replace'),'exitCode':code,'timedOut':timed,'removed':removed}))
`

// AsideCommand is the one-shot launch a side question runs in the guest's
// working directory: the brokered environment, then the guest script
// feeding the question to the CLI, which resumes the session as a copy
// with every tool disabled by the script (it answers, it does not act),
// in the chat's model and output style, and writes the copy nowhere
// (`--no-session-persistence`, which the pinned CLI honours with
// `--fork-session`: it answers from the resumed context and leaves no
// file; the script removes a copy anyway should one appear). Every
// value is data.
func AsideCommand(run RunSpec, question string) []string {
	broker := run.Broker
	paths := run.Paths.orDefaults()
	args := append(claudeEnvironment(broker), "python3", "-c", asideScript, question, strconv.Itoa(int(AsideTimeout/time.Second)), strconv.Itoa(asideOutputLimit), paths.Claude, "-p", "--output-format", "stream-json", "--verbose", "--permission-mode", "default", "--strict-mcp-config", "--setting-sources=", "--no-session-persistence", "--append-system-prompt", "This is a side question asked beside the conversation above; answer it from what you know of the conversation and the workspace, briefly. You cannot run tools or change files here, and nothing you say enters the conversation.")
	if broker.Model != "" {
		args = append(args, "--model", broker.Model)
	}
	return append(args, claudeSessionArgs(broker)...)
}

// OneShotCommand is the one-shot launch a "oneshot" request runs: the
// brokered environment, then the guest script feeding the prompt to a
// fresh CLI with every tool disabled by the script, one turn, no session
// resumed or written (`--no-session-persistence`), the given system
// prompt in place of the CLI's own (a few hundred tokens instead of its
// default's thousands), on the run's model or the request's. Every value
// is data.
func OneShotCommand(run RunSpec, prompt, system string) []string {
	broker := run.Broker
	paths := run.Paths.orDefaults()
	args := append(claudeEnvironment(broker), "python3", "-c", asideScript, prompt, strconv.Itoa(int(OneShotTimeout/time.Second)), strconv.Itoa(asideOutputLimit), paths.Claude, "-p", "--output-format", "stream-json", "--verbose", "--permission-mode", "default", "--strict-mcp-config", "--setting-sources=", "--max-turns", "1", "--no-session-persistence")
	if system != "" {
		args = append(args, "--system-prompt", system)
	}
	if broker.Model != "" {
		args = append(args, "--model", broker.Model)
	}
	return args
}

// aside answers an "aside" request: the question in r.Command, asked of a
// copy of the active run's session (r.ThreadID, the chat's recorded one)
// with the run's brokered environment. The run must be streaming (a
// resident Claude session, idle or not: the copy is taken as the CLI
// finds the session file); the chat decides whether to ask while a turn
// runs.
func (w *Worker) aside(ctx context.Context, r Request) (Response, error) {
	question := r.Command
	if strings.TrimSpace(question) == "" || len(question) > MaxAsideQuestion || strings.ContainsRune(question, 0) {
		return Response{}, errors.New("invalid question")
	}
	if !validIdentity(r.ThreadID) {
		return Response{}, errors.New("the chat has no agent session to ask")
	}
	return w.oneShotRun(ctx, r, AsideTimeout, "side question", func(broker BrokerConfig) (BrokerConfig, func(RunSpec) []string) {
		broker.ThreadID, broker.ForkSession = r.ThreadID, true
		if ValidOutputStyle(r.OutputStyle) {
			broker.OutputStyle = r.OutputStyle
		}
		return broker, func(run RunSpec) []string { return AsideCommand(run, question) }
	})
}

// oneshot answers a "oneshot" request: the prompt in r.Command put to a
// fresh, tool-less CLI on r.Model (the run's model when empty) with the
// system prompt in r.Instructions, beside the active run and with its
// brokered environment, like a side question but resuming nothing. The
// chat's automatic title is one (chats/title.go).
func (w *Worker) oneshot(ctx context.Context, r Request) (Response, error) {
	prompt := r.Command
	if strings.TrimSpace(prompt) == "" || len(prompt) > MaxAsideQuestion || strings.ContainsRune(prompt, 0) {
		return Response{}, errors.New("invalid prompt")
	}
	system := r.Instructions
	if len(system) > MaxAsideQuestion || strings.ContainsRune(system, 0) {
		return Response{}, errors.New("invalid system prompt")
	}
	return w.oneShotRun(ctx, r, OneShotTimeout, "one-shot", func(broker BrokerConfig) (BrokerConfig, func(RunSpec) []string) {
		broker.ThreadID, broker.ForkSession, broker.OutputStyle = "", false, ""
		return broker, func(run RunSpec) []string { return OneShotCommand(run, prompt, system) }
	})
}

// oneShotRun launches a second CLI beside the active run (a resident
// Claude session, idle or not) with the run's brokered environment,
// r.Model when it names a valid one, and the launch `shape` derives from
// the broker; the guest script's report is parsed into the answer. The
// run must be streaming: the gateway serves the credential only then.
func (w *Worker) oneShotRun(ctx context.Context, r Request, timeout time.Duration, what string, shape func(BrokerConfig) (BrokerConfig, func(RunSpec) []string)) (Response, error) {
	w.mu.Lock()
	w.defaultsLocked()
	s, _, err := w.runLocked(r)
	if err == nil && (!s.Active.Streaming || s.Active.broker.Provider != "claude") {
		err = errors.New("the agent's session is not running")
	}
	if err == nil && s.State != "running" {
		err = errors.New("sandbox is stopped")
	}
	if err != nil {
		w.mu.Unlock()
		return Response{}, err
	}
	broker := s.Active.broker
	if r.Model != "" && ValidateAgent("claude", r.Model) == nil {
		broker.Model = r.Model
	}
	broker, command := shape(broker)
	run := RunSpec{Directory: s.Directory, Broker: broker, Paths: s.paths.orDefaults()}
	name := s.RuntimeName
	s.LastActivity = w.now()
	w.mu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, timeout+20*time.Second)
	defer cancel()
	raw, err := w.Runtime.Exec(ctx, name, run.Directory, command(run)...)
	if err != nil {
		if ctx.Err() != nil {
			return Response{}, errors.New("the " + what + " did not finish in time")
		}
		return Response{}, errors.New("could not run the " + what + " in the sandbox")
	}
	var report struct {
		Output   string   `json:"output"`
		Stderr   string   `json:"stderr"`
		ExitCode int      `json:"exitCode"`
		TimedOut bool     `json:"timedOut"`
		Removed  []string `json:"removed"`
	}
	if err = json.Unmarshal([]byte(raw), &report); err != nil {
		return Response{}, errors.New("invalid sandbox " + what + " response")
	}
	result := ParseAsideOutput(report.Output)
	result.Removed = len(report.Removed)
	if result.Removed > 0 {
		log.Printf("sandbox %s: the %s's session copy removed from the guest (%d files)", name, what, result.Removed)
	}
	switch {
	case report.TimedOut:
		result.Error = "the answer did not arrive within " + timeout.String()
	case result.Text == "" && result.Error == "":
		result.Error = asideFailure(report.ExitCode, report.Stderr)
	}
	return Response{Aside: &result}, nil
}

// ParseAsideOutput reads the one-shot CLI's stream-json: the `result`
// frame's text, cost, usage, duration and session id (an error result's
// text is the error), with the assistant frames' text as the answer when
// the result carries none.
func ParseAsideOutput(output string) AsideResult {
	var result AsideResult
	var texts []string
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var v map[string]any
		if json.Unmarshal([]byte(line), &v) != nil {
			continue
		}
		switch v["type"] {
		case "assistant":
			m, _ := v["message"].(map[string]any)
			content, _ := m["content"].([]any)
			for _, b := range content {
				block, _ := b.(map[string]any)
				if block["type"] == "text" {
					if s, _ := block["text"].(string); s != "" {
						texts = append(texts, s)
					}
				}
			}
		case "result":
			text, _ := v["result"].(string)
			if v["is_error"] == true {
				result.Error = text
				if result.Error == "" {
					result.Error = "Claude could not answer"
				}
			} else {
				result.Text = text
			}
			result.CostUSD, _ = v["total_cost_usd"].(float64)
			if d, ok := v["duration_ms"].(float64); ok {
				result.DurationMS = int64(d)
			}
			result.SessionID, _ = v["session_id"].(string)
			if u, ok := v["usage"].(map[string]any); ok {
				n := func(k string) int64 { f, _ := u[k].(float64); return int64(f) }
				result.Input = n("input_tokens") + n("cache_creation_input_tokens") + n("cache_read_input_tokens")
				result.Output = n("output_tokens")
			}
		}
	}
	if result.Text == "" && result.Error == "" && len(texts) > 0 {
		result.Text = strings.Join(texts, "\n\n")
	}
	return result
}

// asideFailure words a one-shot that produced no answer: the CLI's last
// stderr line when it said something, else its exit code.
func asideFailure(code int, stderr string) string {
	lines := strings.Split(strings.TrimSpace(stderr), "\n")
	if last := strings.TrimSpace(lines[len(lines)-1]); last != "" && !strings.HasPrefix(last, "Warning: no stdin") {
		if len(last) > 300 {
			last = last[:300] + "…"
		}
		return last
	}
	if code == 0 {
		return "Claude gave no answer"
	}
	return "Claude exited with code " + strconv.Itoa(code)
}

// OutputStyles are the Claude output styles a chat may launch with: the
// CLI's built-ins (docs/claude-parity.md, item 15). "" is the default.
var OutputStyles = []string{"Explanatory", "Learning"}

var outputStyleName = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9 _-]{0,63}$`)

// ValidOutputStyle reports whether style is one a chat may launch with:
// "" (the default) or one of OutputStyles. The pattern check keeps a
// future custom style's name data-shaped.
func ValidOutputStyle(style string) bool {
	if style == "" {
		return true
	}
	if !outputStyleName.MatchString(style) {
		return false
	}
	for _, s := range OutputStyles {
		if s == style {
			return true
		}
	}
	return false
}
