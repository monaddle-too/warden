package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"warden/chat/internal/sandbox"
	"warden/chat/internal/tui"
)

const chatUsage = `usage: warden chat [flags] [CHAT]           interactive terminal client
       warden chat list [flags]               list chats
       warden chat new [flags] [TITLE]        create a chat and print its id
       warden chat send [flags] CHAT TEXT     send a message (--wait streams the reply)
       warden chat approve [flags] CHAT [--decline] [--answer TEXT]
                                              answer the first pending approval

CHAT is a chat id, an id prefix, a title, or a number from 'warden chat list'.
Flags: --config PATH, --state DIR, --provider codex|claude, --model NAME,
       --cpus N and --memory SIZE (new: the fresh workspace's size, e.g.
       --cpus 2 --memory 4g; default: the runner's, whole CPUs on SBX).
`

// endpoint reads the running chat service's URL and capability.
func endpoint(file string) (base, token string, err error) {
	raw, err := os.ReadFile(file)
	if err != nil {
		return "", "", fmt.Errorf("%w; run `warden start` first (it stays in the foreground), then `warden chat` in another terminal", err)
	}
	var e struct{ URL, Token string }
	if err = json.Unmarshal(raw, &e); err != nil || e.URL == "" || e.Token == "" {
		return "", "", errors.New(file + " is not a chat endpoint file")
	}
	return e.URL, e.Token, nil
}

func (c *cli) chat(args []string) error {
	sub := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		switch args[0] {
		case "list", "new", "send", "approve", "help":
			sub, args = args[0], args[1:]
		}
	}
	if sub == "help" {
		fmt.Fprint(c.stdout, chatUsage)
		return nil
	}
	fs := flag.NewFlagSet("warden chat", flag.ContinueOnError)
	fs.SetOutput(c.stderr)
	fs.Usage = func() { fmt.Fprint(c.stderr, chatUsage) }
	configPath := fs.String("config", "", "warden.json (default: <state>/warden.json or $WARDEN_CONFIG)")
	state := fs.String("state", "", "state directory when no warden.json exists yet")
	provider := fs.String("provider", "", "provider for a new chat: codex or claude (default: codex)")
	model := fs.String("model", "", "model for a new chat (default: the provider's default)")
	cpus := fs.Float64("cpus", 0, "new: CPUs for the fresh workspace (default: the runner's)")
	memory := fs.String("memory", "", "new: memory for the fresh workspace, e.g. 4g or 2048m (default: the runner's)")
	wait := fs.Bool("wait", false, "send: stream the transcript until the agent is idle")
	decline := fs.Bool("decline", false, "approve: decline instead of allowing")
	answer := fs.String("answer", "", "approve: the answer to the agent's question")
	all := fs.Bool("all", false, "list: include archived chats")
	if err := fs.Parse(interleaved(args, map[string]bool{"config": true, "state": true, "provider": true, "model": true, "answer": true, "cpus": true, "memory": true})); err != nil {
		return errUsage
	}
	cfg, _, err := loadConfig(*configPath, *state)
	if err != nil {
		return err
	}
	base, token, err := endpoint(cfg.OwnerTokenFile())
	if err != nil {
		return err
	}
	client := &tui.Client{Base: base, Token: token}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	switch sub {
	case "list":
		s, err := client.State(ctx)
		if err != nil {
			return err
		}
		n := 0
		for _, ch := range s.Chats {
			if ch.Archived && !*all {
				continue
			}
			n++
			fmt.Fprintf(c.stdout, "%s  %s\n", tui.ChatLine(n, ch), ch.ID)
		}
		if n == 0 {
			fmt.Fprintln(c.stdout, "no chats; warden chat new [TITLE]")
		}
		return nil
	case "new":
		// No title: the chat is named from its first exchange (the
		// service's automatic titles, chats/title.go).
		title := strings.Join(fs.Args(), " ")
		p := *provider
		if p == "" {
			p = "codex"
		}
		var resources *sandbox.Resources
		if *cpus != 0 || *memory != "" {
			resources = &sandbox.Resources{CPUMilli: sandbox.CPUMilli(*cpus)}
			if *cpus != 0 && resources.CPUMilli == 0 {
				return errors.New("--cpus must be a positive number")
			}
			if *memory != "" {
				if resources.MemoryMB, err = sandbox.ParseMemoryMB(*memory); err != nil {
					return err
				}
			}
		}
		id, err := client.Create(ctx, title, p, *model, "", resources)
		if err != nil {
			return err
		}
		fmt.Fprintln(c.stdout, id)
		return nil
	case "send":
		if fs.NArg() < 2 {
			fmt.Fprint(c.stderr, chatUsage)
			return errUsage
		}
		id, err := resolveChat(ctx, client, fs.Arg(0))
		if err != nil {
			return err
		}
		text := strings.Join(fs.Args()[1:], " ")
		seen := map[string]bool{}
		if *wait {
			if s, err := client.State(ctx); err == nil {
				if ch := s.Chat(id); ch != nil {
					for _, e := range ch.Conversation.Entries {
						seen[e.ID] = true
					}
				}
			}
		}
		if err := client.Message(ctx, id, text, tui.NewMessageID()); err != nil {
			return err
		}
		if !*wait {
			fmt.Fprintln(c.stdout, "sent")
			return nil
		}
		status, err := tui.Follow(ctx, client, id, c.stdout, seen)
		if err != nil {
			return err
		}
		if status != "" && status != "idle" {
			fmt.Fprintf(c.stdout, "chat is %s\n", status)
		}
		return nil
	case "approve":
		if fs.NArg() < 1 {
			fmt.Fprint(c.stderr, chatUsage)
			return errUsage
		}
		id, err := resolveChat(ctx, client, fs.Arg(0))
		if err != nil {
			return err
		}
		s, err := client.State(ctx)
		if err != nil {
			return err
		}
		ch := s.Chat(id)
		pending := ch.Pending()
		if len(pending) == 0 {
			return errors.New("no pending approval")
		}
		first := pending[0]
		var answers map[string][]string
		if qs := first.Questions(); len(qs) > 0 && !*decline {
			if *answer == "" {
				return errors.New("the agent asked a question; pass --answer TEXT or --decline")
			}
			answers = map[string][]string{}
			for _, q := range qs {
				answers[q.ID] = []string{*answer}
			}
		}
		if err := client.Resolve(ctx, id, first.ID, !*decline, answers); err != nil {
			return err
		}
		if *decline {
			fmt.Fprintf(c.stdout, "declined %s\n", first.Method)
		} else {
			fmt.Fprintf(c.stdout, "allowed %s\n", first.Method)
		}
		return nil
	}
	// Interactive.
	app := &tui.App{Client: client, Provider: *provider, OpenURL: openBrowser, Clipboard: copyToClipboard, HistoryDir: filepath.Join(cfg.Paths.State, "tui", "history"), BellFile: filepath.Join(cfg.Paths.State, "tui", "bell")}
	if url, err := launchURL(cfg.OwnerTokenFile(), time.Now()); err == nil {
		if cfg.Auth.Mode == "owner" && cfg.Auth.PublicURL != "" {
			url, _ = throughEdge(url, cfg.Auth.PublicURL)
		}
		app.AppURL = url
	}
	if fs.NArg() > 0 {
		id, err := resolveChat(ctx, client, strings.Join(fs.Args(), " "))
		if err != nil {
			return err
		}
		app.ChatID = id
	} else if s, err := client.State(ctx); err == nil {
		app.ChatID = mostRecentlyActive(s)
	}
	return tui.RunOnTerminal(ctx, app)
}

// resolveChat accepts an id, an unambiguous id prefix, an exact title, or a
// 1-based number from `warden chat list`.
func resolveChat(ctx context.Context, client *tui.Client, ref string) (string, error) {
	s, err := client.State(ctx)
	if err != nil {
		return "", err
	}
	var live []*tui.Chat
	for _, ch := range s.Chats {
		if !ch.Archived {
			live = append(live, ch)
		}
	}
	if n, err := strconv.Atoi(ref); err == nil && n >= 1 && n <= len(live) {
		return live[n-1].ID, nil
	}
	var matches []*tui.Chat
	for _, ch := range s.Chats {
		if ch.ID == ref || ch.Title == ref {
			return ch.ID, nil
		}
		if strings.HasPrefix(ch.ID, ref) {
			matches = append(matches, ch)
		}
	}
	switch len(matches) {
	case 1:
		return matches[0].ID, nil
	case 0:
		return "", fmt.Errorf("no chat matches %q; see `warden chat list`", ref)
	default:
		return "", fmt.Errorf("%q matches %d chats; use a longer id", ref, len(matches))
	}
}

// interleaved moves flags that follow positional arguments in front of them,
// so `warden chat send 1 "text" --wait` works as people type it. valueFlags
// names the flags that take the next argument as their value when written
// without "=".
func interleaved(args []string, valueFlags map[string]bool) []string {
	var flags, positional []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			positional = append(positional, args[i+1:]...)
			break
		}
		if !strings.HasPrefix(a, "-") || a == "-" {
			positional = append(positional, a)
			continue
		}
		flags = append(flags, a)
		name := strings.TrimLeft(a, "-")
		if !strings.Contains(name, "=") && valueFlags[name] && i+1 < len(args) {
			i++
			flags = append(flags, args[i])
		}
	}
	return append(flags, positional...)
}

// mostRecentlyActive picks the unarchived chat with the newest transcript
// entry, falling back to the most recently created one, so `warden chat`
// opens where the person last worked rather than an empty new chat.
func mostRecentlyActive(s *tui.State) string {
	best, bestAt := "", -1.0
	for _, ch := range s.Chats {
		if ch.Archived {
			continue
		}
		at := 0.0
		for _, e := range ch.Conversation.Entries {
			if e.CreatedAt > at {
				at = e.CreatedAt
			}
		}
		if at > bestAt {
			best, bestAt = ch.ID, at
		}
	}
	return best
}
